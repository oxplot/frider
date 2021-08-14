package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	htemplate "html/template"
	"log"
	"net"
	"net/smtp"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	ttemplate "text/template"
	"time"

	_ "github.com/joho/godotenv/autoload"
	"github.com/mmcdole/gofeed"
	"gopkg.in/yaml.v3"
)

const (
	sameDomainRequestDelay = time.Second * 2
)

type Config struct {
	Storage struct {
		Path       string `yaml:"path"`
		ItemUID    string `yaml:"item_uid"`
		itemUIDTpl *ttemplate.Template
		FeedUID    string `yaml:"feed_uid"`
		feedUIDTpl *ttemplate.Template
	} `yaml:"storage"`
	SMTP struct {
		Sender    string `yaml:"sender"`
		senderTpl *ttemplate.Template
		Recipient string `yaml:"recipient"`
		Address   string `yaml:"address"`
		host      string
		port      string
		Username  string `yaml:"username"`
		Password  string `yaml:"password"`
		Jobs      int    `yaml:"jobs"`
	} `yaml:"smtp"`
	Email struct {
		Subject    string `yaml:"subject"`
		subjectTpl *ttemplate.Template
		Content    string `yaml:"content"`
		contentTpl *htemplate.Template
	} `yaml:"email"`
	Feeds []*FeedSpec `yaml:"feeds"`
}

type FeedSpec struct {
	Name      string `yaml:"name"`
	URL       string `yaml:"url"`
	parsedURL *url.URL
}

type feedItem struct {
	Feed     *gofeed.Feed
	Item     *gofeed.Item
	FeedSpec *FeedSpec
	Config   *Config
}

var (
	configPath = flag.String("config", os.Getenv("FRIDER_CONFIG"), "path to config file")
	config     *Config
	store      *storage

	emailPat = regexp.MustCompile(`<([^>@]+@[^>]+)>`)
)

type storage struct {
	path string
}

func (s *storage) keyPath(k string) string {
	return filepath.Join(s.path, k[:2], k[2:])
}

func (s *storage) has(k string) bool {
	kp := s.keyPath(k)
	_, err := os.Stat(kp)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("warn: cannot stat key '%s' in storage: %s", kp, err)
		}
		return false
	}
	return true
}

func (s *storage) set(k string) {
	kp := s.keyPath(k)
	d := filepath.Dir(kp)
	if err := os.MkdirAll(d, os.ModePerm); err != nil {
		log.Printf("warn: cannot create dir '%s' in storage: %s", d, err)
		return
	}
	f, err := os.Create(kp)
	if err != nil {
		log.Printf("cannot create key '%s' in storage: %s", kp, err)
		return
	}
	f.Close()
}

func extractEmail(addr string) string {
	m := emailPat.FindStringSubmatch(addr)
	if m == nil {
		return ""
	}
	return m[1]
}

func sendEmail(from, msg string) error {
	auth := smtp.PlainAuth("", config.SMTP.Username, config.SMTP.Password, config.SMTP.host)
	tlsconfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         config.SMTP.host,
	}
	c, err := smtp.Dial(config.SMTP.Address)
	if err != nil {
		return err
	}
	c.StartTLS(tlsconfig)
	if err = c.Auth(auth); err != nil {
		return err
	}
	if err = c.Mail(extractEmail(from)); err != nil {
		return err
	}
	if err = c.Rcpt(extractEmail(config.SMTP.Recipient)); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(msg))
	if err != nil {
		return err
	}
	err = w.Close()
	if err != nil {
		return err
	}
	c.Quit()
	return nil
}

func sendEmails(c chan feedItem, done func()) {
	var buf bytes.Buffer

	for fi := range c {
		uid, _ := calcItemUID(fi) // processDomainFeeds ensures we don't get errors here

		if err := config.SMTP.senderTpl.Execute(&buf, fi); err != nil {
			log.Printf("warn: failed to render sender tpl: %s", err)
			continue
		}
		sender := string(buf.Bytes())
		buf.Reset()

		if err := config.Email.subjectTpl.Execute(&buf, fi); err != nil {
			log.Printf("warn: failed to render subject tpl: %s", err)
			continue
		}
		subject := string(buf.Bytes())
		buf.Reset()

		if err := config.Email.contentTpl.Execute(&buf, fi); err != nil {
			log.Printf("warn: failed to render content tpl: %s", err)
			continue
		}
		content := string(buf.Bytes())
		buf.Reset()

		msg := fmt.Sprintf(
			"From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/html;charset=utf8\r\n\r\n%s",
			sender, config.SMTP.Recipient, subject, content,
		)

		if err := sendEmail(sender, msg); err != nil {
			log.Printf("warn: failed to send email: %s", err)
			continue
		}

		store.set(uid)
	}

	done()
}

func calcItemUID(i feedItem) (string, error) {
	var buf bytes.Buffer
	if err := config.Storage.itemUIDTpl.Execute(&buf, i); err != nil {
		return "", fmt.Errorf("cannot calculate item UID")
	}
	return fmt.Sprintf("%x", sha256.Sum256(buf.Bytes())), nil
}

func calcFeedUID(i feedItem) (string, error) {
	var buf bytes.Buffer
	if err := config.Storage.feedUIDTpl.Execute(&buf, i); err != nil {
		return "", fmt.Errorf("cannot calculate feed UID")
	}
	return fmt.Sprintf("%x", sha256.Sum256(buf.Bytes())), nil
}

func processDomainFeeds(feedChan chan *FeedSpec, itemChan chan feedItem, done func()) {
	parser := gofeed.NewParser()
	for fs := range feedChan {
		time.Sleep(sameDomainRequestDelay)

		f, err := parser.ParseURL(fs.URL)
		if err != nil {
			log.Printf("warn: failed to parse feed '%s' url '%s': %s", fs.Name, fs.URL, err)
			continue
		}

		feedUID, err := calcFeedUID(feedItem{Feed: f, FeedSpec: fs, Config: config})
		if err != nil {
			log.Printf("warn: failed to calculate feed uid in feed '%s': %s", fs.Name, err)
			continue
		}
		seenFeed := store.has(feedUID)

		for _, i := range f.Items {
			fi := feedItem{Feed: f, Item: i, FeedSpec: fs, Config: config}
			itemUID, err := calcItemUID(fi)
			if err != nil {
				log.Printf("warn: failed to calculate item uid in feed '%s': %s", fs.Name, err)
				continue
			}
			if seenFeed {
				if store.has(itemUID) {
					continue
				}
				itemChan <- fi
			} else {
				store.set(itemUID)
			}
		}

		store.set(feedUID)
	}
	done()
}

func run() error {
	var err error
	if config, err = loadConfig(*configPath); err != nil {
		return fmt.Errorf("failed to load config: %s", err)
	}
	store = &storage{path: config.Storage.Path}

	emailerWG := sync.WaitGroup{}
	itemChan := make(chan feedItem)
	emailerWG.Add(config.SMTP.Jobs)
	for i := 0; i < config.SMTP.Jobs; i++ {
		go sendEmails(itemChan, emailerWG.Done)
	}

	domainWG := sync.WaitGroup{}
	domains := map[string]chan *FeedSpec{}

	for _, f := range config.Feeds {
		u, err := url.Parse(f.URL)
		if err != nil {
			log.Printf("warn: cannot parse '%s' feed URL '%s': %s", f.Name, f.URL, err)
			continue
		}
		f.parsedURL = u

		feedChan, ok := domains[f.parsedURL.Host]
		if !ok {
			domainWG.Add(1)
			feedChan = make(chan *FeedSpec)
			domains[f.parsedURL.Host] = feedChan
			go processDomainFeeds(feedChan, itemChan, domainWG.Done)
		}
		feedChan <- f
	}

	for _, d := range domains {
		close(d)
	}

	domainWG.Wait()
	close(itemChan)
	emailerWG.Wait()

	return nil
}

func loadConfig(path string) (*Config, error) {
	var c Config
	feedLinkTpl := "{{or .Feed.Link .Feed.FeedLink .FeedSpec.URL}}"
	c.Storage.ItemUID = feedLinkTpl + "|{{.Item.Title}}|{{or .Item.GUID .Item.Link}}"
	c.Storage.FeedUID = feedLinkTpl
	c.SMTP.Jobs = 4
	c.Email.Subject = "{{.Item.Title}}"
	c.Email.Content = "<h2><a href=\"{{.Item.Link}}\">{{.Item.Title}}</a></h2>{{.Item.Content | noescape}}"

	var f *os.File
	var err error
	if path == "-" {
		f = os.Stdin
	} else {
		f, err = os.Open(*configPath)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	y := yaml.NewDecoder(f)
	if err = y.Decode(&c); err != nil {
		return nil, err
	}

	c.Storage.itemUIDTpl, err = ttemplate.New("").Parse(c.Storage.ItemUID)
	if err != nil {
		return nil, fmt.Errorf("can't parse storage.item_uid '%s': %s", c.Storage.ItemUID, err)
	}
	c.Storage.feedUIDTpl, err = ttemplate.New("").Parse(c.Storage.FeedUID)
	if err != nil {
		return nil, fmt.Errorf("can't parse storage.feed_uid '%s': %s", c.Storage.FeedUID, err)
	}
	c.SMTP.senderTpl, err = ttemplate.New("").Parse(c.SMTP.Sender)
	if err != nil {
		return nil, fmt.Errorf("can't parse smtp.sender '%s': %s", c.SMTP.Sender, err)
	}
	c.Email.subjectTpl, err = ttemplate.New("").Parse(c.Email.Subject)
	if err != nil {
		return nil, fmt.Errorf("can't parse email.subject '%s': %s", c.Email.Subject, err)
	}
	c.Email.contentTpl, err = htemplate.New("").Funcs(htemplate.FuncMap{
		"noescape": func(s string) htemplate.HTML {
			return htemplate.HTML(s)
		},
	}).Parse(c.Email.Content)
	if err != nil {
		return nil, fmt.Errorf("can't parse email.content '%s': %s", c.Email.Content, err)
	}
	if _, err = os.Stat(filepath.Join(c.Storage.Path, ".frider")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("invalid storage path - must have .frider file inside: %s", err)
	}
	c.SMTP.host, c.SMTP.port, err = net.SplitHostPort(c.SMTP.Address)
	if err != nil {
		return nil, fmt.Errorf("can't parse smtp.address '%s': %s", c.SMTP.Address, err)
	}

	return &c, nil
}

func main() {
	log.SetFlags(0)
	flag.Parse()
	if err := run(); err != nil {
		log.Fatalf("error: %s", err)
	}
}
