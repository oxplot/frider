package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	htemplate "html/template"
	"log"
	"net/url"
	"os"
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
		Path        string `yaml:"path"`
		UID         string `yaml:"uid"`
		uidTemplate *ttemplate.Template
	} `yaml:"storage"`
	SMTP struct {
		Sender     string `yaml:"sender"`
		senderTpl  *ttemplate.Template
		Recipients []string `yaml:"recipients"`
		Host       string   `yaml:"host"`
		Port       int      `yaml:"port"`
		Username   string   `yaml:"username"`
		Password   string   `yaml:"password"`
		Jobs       int      `yaml:"jobs"`
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
	Feed *gofeed.Feed
	Item *gofeed.Item
}

var (
	configPath = flag.String("config", os.Getenv("FRIDER_CONFIG"), "path to config file")
	config     *Config
)

func sendEmails(c chan feedItem, done func()) {
	for fi := range c {
		log.Printf("sending email: %s %s", fi.Feed.Title, fi.Item.Title)
	}
	done()
}

func calcItemUID(i feedItem) (string, error) {
	var buf bytes.Buffer
	if err := config.Storage.uidTemplate.Execute(&buf, i); err != nil {
		return "", fmt.Errorf("cannot calculate UID")
	}
	return fmt.Sprintf("%x", sha256.Sum256(buf.Bytes())), nil
}

func processDomainFeeds(feedChan chan *FeedSpec, emailChan chan feedItem, done func()) {
	parser := gofeed.NewParser()
	for fs := range feedChan {
		time.Sleep(sameDomainRequestDelay)
		f, err := parser.ParseURL(fs.URL)
		if err != nil {
			log.Printf("warn: failed to parse feed '%s' url '%s': %s", fs.Name, fs.URL, err)
			continue
		}
		for _, i := range f.Items {
			fi := feedItem{Feed: f, Item: i}
			uid, err := calcItemUID(fi)
			if err != nil {
				log.Printf("warn: failed to calculate item uid in feed '%s': %s", fs.Name, err)
				continue
			}
			// TODO check storage
			_ = uid
			emailChan <- fi
		}
	}
	done()
}

func run() error {
	var err error
	if config, err = loadConfig(*configPath); err != nil {
		return fmt.Errorf("failed to load config: %s", err)
	}

	emailerWG := sync.WaitGroup{}
	emailChan := make(chan feedItem)
	emailerWG.Add(config.SMTP.Jobs)
	for i := 0; i < config.SMTP.Jobs; i++ {
		go sendEmails(emailChan, emailerWG.Done)
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
			go processDomainFeeds(feedChan, emailChan, domainWG.Done)
		}
		feedChan <- f
	}

	for _, d := range domains {
		close(d)
	}

	domainWG.Wait()
	close(emailChan)
	emailerWG.Wait()

	return nil
}

func loadConfig(path string) (*Config, error) {
	var c Config
	c.Storage.UID = "{{.Feed.Link}}|{{.Feed.FeedLink}}|{{.Item.Title}}|{{.Item.Link}}|{{.Item.GUID}}|{{.Item.Content}}"
	c.SMTP.Jobs = 4
	c.Email.Subject = "{{.Item.Title}}"
	c.Email.Content = "<h2><a href=\"{{.Item.Link}}\">{{.Item.Title}}</a></h2>{{.Item.Html}}"

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
	c.Storage.uidTemplate, err = ttemplate.New("").Parse(c.Storage.UID)
	if err != nil {
		return nil, fmt.Errorf("can't parse storage.uid '%s': %s", c.Storage.UID, err)
	}
	c.SMTP.senderTpl, err = ttemplate.New("").Parse(c.SMTP.Sender)
	if err != nil {
		return nil, fmt.Errorf("can't parse smtp.sender '%s': %s", c.SMTP.Sender, err)
	}
	c.Email.subjectTpl, err = ttemplate.New("").Parse(c.Email.Subject)
	if err != nil {
		return nil, fmt.Errorf("can't parse email.subject '%s': %s", c.Email.Subject, err)
	}
	c.Email.contentTpl, err = htemplate.New("").Parse(c.Email.Content)
	if err != nil {
		return nil, fmt.Errorf("can't parse email.content '%s': %s", c.Email.Content, err)
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
