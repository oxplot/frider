package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	htemplate "html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"os/exec"
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
	useragent              = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/92.0.4515.131 Safari/537.36"
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
	Exec  struct {
		Jobs int `yaml:"jobs"`
	} `yaml:"exec"`
}

func NewConfig() *Config {
	var c Config
	feedLinkTpl := "{{or .Feed.FeedLink .FeedSpec.URL}}"
	c.Storage.ItemUID = feedLinkTpl + "|{{or .Item.GUID .Item.Link}}"
	c.Storage.FeedUID = feedLinkTpl
	c.SMTP.Jobs = 4
	c.Exec.Jobs = 4
	c.Email.Subject = "{{.Item.Title | nonewlines}}"
	c.Email.Content = `<h2><a href="{{.Item.Link}}">{{.Item.Title}}</a></h2>
{{with $c := (or .Item.Content .Item.Description)}}
  {{if (ishtml $c)}}
    {{$c | noescape}}
  {{else}}
    <p style="white-space:pre-wrap">{{$c}}</p>
  {{end}}
{{end}}
`
	return &c
}

func (c *Config) Load(r io.Reader) error {
	var err error
	y := yaml.NewDecoder(r)
	if err = y.Decode(&c); err != nil {
		return err
	}

	c.Storage.itemUIDTpl, err = ttemplate.New("").Funcs(tplFuncs).Parse(c.Storage.ItemUID)
	if err != nil {
		return fmt.Errorf("can't parse storage.item_uid '%s': %s", c.Storage.ItemUID, err)
	}
	c.Storage.feedUIDTpl, err = ttemplate.New("").Funcs(tplFuncs).Parse(c.Storage.FeedUID)
	if err != nil {
		return fmt.Errorf("can't parse storage.feed_uid '%s': %s", c.Storage.FeedUID, err)
	}
	c.SMTP.senderTpl, err = ttemplate.New("").Funcs(tplFuncs).Parse(c.SMTP.Sender)
	if err != nil {
		return fmt.Errorf("can't parse smtp.sender '%s': %s", c.SMTP.Sender, err)
	}
	c.Email.subjectTpl, err = ttemplate.New("").Funcs(tplFuncs).Parse(c.Email.Subject)
	if err != nil {
		return fmt.Errorf("can't parse email.subject '%s': %s", c.Email.Subject, err)
	}
	c.Email.contentTpl, err = htemplate.New("").Funcs(tplFuncs).Parse(c.Email.Content)
	if err != nil {
		return fmt.Errorf("can't parse email.content '%s': %s", c.Email.Content, err)
	}
	if _, err = os.Stat(filepath.Join(c.Storage.Path, ".frider")); err != nil {
		return fmt.Errorf("invalid storage path - must have .frider file inside: %s", err)
	}
	c.SMTP.host, c.SMTP.port, err = net.SplitHostPort(c.SMTP.Address)
	if err != nil {
		return fmt.Errorf("can't parse smtp.address '%s': %s", c.SMTP.Address, err)
	}
	if c.SMTP.Password == "" {
		c.SMTP.Password = os.Getenv("FRIDER_SMTP_PASSWORD")
	}

	return nil
}

func (c *Config) LoadFile(path string) error {
	var err error
	var f *os.File
	if path == "-" {
		f = os.Stdin
	} else {
		f, err = os.Open(*configPath)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	return c.Load(f)
}

func (c *Config) Save(w io.Writer) error {
	return yaml.NewEncoder(w).Encode(c)
}

type FeedSpec struct {
	Name          string   `yaml:"name"`
	URL           string   `yaml:"url"`
	SkipTLSVerify bool     `yaml:"skip_tls_verify"`
	Exec          []string `yaml:"exec"`
	parsedURL     *url.URL
}

type feedItem struct {
	Feed     *gofeed.Feed
	Item     *gofeed.Item
	FeedSpec *FeedSpec
	Config   *Config
}

var (
	configPath         = flag.String("config", os.Getenv("FRIDER_CONFIG"), "path to config file")
	printDefaultConfig = flag.Bool("print-default-config", false, "print default config and exit")
	config             *Config
	store              *storage

	newlinePat = regexp.MustCompile(`[\r\n]+`)
	emailPat   = regexp.MustCompile(`<([^>@]+@[^>]+)>`)
	htmlTagPat = regexp.MustCompile(`(?i)<(img|br|hr)[^>]*>|</[a-z]+>|&([a-z]+|[#]\d+);`)
	tplFuncs   = map[string]interface{}{
		"ishtml": func(s string) bool {
			return htmlTagPat.MatchString(s)
		},
		"noescape": func(s string) htemplate.HTML {
			return htemplate.HTML(s)
		},
		"nonewlines": func(s string) string {
			return newlinePat.ReplaceAllString(s, " ")
		},
	}
)

type storage struct {
	path string
}

func (s *storage) keyPath(k string) string {
	h := fmt.Sprintf("%x", sha256.Sum256([]byte(k)))
	return filepath.Join(s.path, h[:2], h)
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
	defer f.Close()
	if _, err := f.Write([]byte(k)); err != nil {
		log.Printf("cannot write content of key '%s' in storage: %s", kp, err)
		return
	}
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
	defer done()
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
		log.Printf("info: sent feed email: %s - %s", fi.FeedSpec.Name, fi.Item.Title)

		store.set(uid)
	}
}

func calcItemUID(i feedItem) (string, error) {
	var buf bytes.Buffer
	if err := config.Storage.itemUIDTpl.Execute(&buf, i); err != nil {
		return "", fmt.Errorf("cannot calculate item UID")
	}
	return string(buf.Bytes()), nil
}

func calcFeedUID(i feedItem) (string, error) {
	var buf bytes.Buffer
	if err := config.Storage.feedUIDTpl.Execute(&buf, i); err != nil {
		return "", fmt.Errorf("cannot calculate feed UID")
	}
	return string(buf.Bytes()), nil
}

func processDomainFeeds(feedChan chan *FeedSpec, itemChan chan feedItem, done func()) {
	defer done()
	for fs := range feedChan {
		time.Sleep(sameDomainRequestDelay)
		log.Printf("info: processing url feed: %s", fs.Name)

		parser := gofeed.NewParser()
		parser.UserAgent = useragent
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: fs.SkipTLSVerify},
		}
		parser.Client = &http.Client{Transport: tr}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		f, err := parser.ParseURLWithContext(fs.URL, ctx)
		cancel()
		if err != nil {
			log.Printf("warn: failed to parse feed '%s' url '%s': %s", fs.Name, fs.URL, err)
			continue
		}

		if err := processFeeds(fs, f, itemChan); err != nil {
			log.Printf("warn: %s", err)
		}
	}
}

func processFeeds(fs *FeedSpec, f *gofeed.Feed, itemChan chan feedItem) error {
	feedUID, err := calcFeedUID(feedItem{Feed: f, FeedSpec: fs, Config: config})
	if err != nil {
		return fmt.Errorf("failed to calculate feed uid in feed '%s': %s", fs.Name, err)
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
	return nil
}

func processExecFeeds(feedChan chan *FeedSpec, itemChan chan feedItem, done func()) {
	defer done()
	parser := gofeed.NewParser()
	for fs := range feedChan {
		log.Printf("info: processing exec feed: %s", fs.Name)

		b, err := exec.Command(fs.Exec[0], fs.Exec[1:]...).Output()
		if err != nil {
			var errStr string
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				errStr = string(exitErr.Stderr)
			} else {
				errStr = err.Error()
			}
			log.Printf("warn: failed to run exec feed '%s' successfully: %s", fs.Name, errStr)
			continue
		}

		r := bytes.NewReader(b)
		f, err := parser.Parse(r)
		if err != nil {
			log.Printf("warn: failed to parse exec feed '%s': %s", fs.Name, err)
			continue
		}

		if err := processFeeds(fs, f, itemChan); err != nil {
			log.Printf("warn: %s", err)
		}
	}
}

func run() error {
	var err error
	config = NewConfig()
	if *printDefaultConfig {
		config.Save(os.Stdout)
		return nil
	}

	if err = config.LoadFile(*configPath); err != nil {
		return fmt.Errorf("failed to load config: %s", err)
	}
	store = &storage{path: config.Storage.Path}

	emailerWG := sync.WaitGroup{}
	itemChan := make(chan feedItem, 1000)
	emailerWG.Add(config.SMTP.Jobs)
	for i := 0; i < config.SMTP.Jobs; i++ {
		go sendEmails(itemChan, emailerWG.Done)
	}

	procWG := sync.WaitGroup{}

	execFeedCh := make(chan *FeedSpec, 1000)
	procWG.Add(config.Exec.Jobs)
	for i := 0; i < config.Exec.Jobs; i++ {
		go processExecFeeds(execFeedCh, itemChan, procWG.Done)
	}

	domains := map[string]chan *FeedSpec{}

	for _, f := range config.Feeds {
		u, err := url.Parse(f.URL)
		if err != nil {
			log.Printf("warn: cannot parse '%s' feed URL '%s': %s", f.Name, f.URL, err)
			continue
		}
		f.parsedURL = u

		if len(f.Exec) > 0 {
			execFeedCh <- f
			continue
		}

		urlFeedChan, ok := domains[f.parsedURL.Host]
		if !ok {
			procWG.Add(1)
			urlFeedChan = make(chan *FeedSpec, 1000)
			domains[f.parsedURL.Host] = urlFeedChan
			go processDomainFeeds(urlFeedChan, itemChan, procWG.Done)
		}
		urlFeedChan <- f
	}

	close(execFeedCh)
	for _, d := range domains {
		close(d)
	}

	procWG.Wait()
	close(itemChan)
	emailerWG.Wait()

	return nil
}

func main() {
	log.SetFlags(0)
	flag.Parse()
	if err := run(); err != nil {
		log.Fatalf("error: %s", err)
	}
}
