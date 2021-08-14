package main

import (
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"

	_ "github.com/gorilla/feeds"
	_ "github.com/joho/godotenv/autoload"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Feeds []*FeedSpec `yaml:"feeds"`
	SMTP  struct {
		Sender     string   `yaml:"sender"`
		Recipients []string `yaml:"recipients"`
		Host       string   `yaml:"host"`
		Port       string   `yaml:"port"`
		Username   string   `yaml:"username"`
		Password   string   `yaml:"password"`
	} `yaml:"smtp"`
	Email struct {
		Subject     string `yaml:"subject"`
		HTMLContent string `yaml:"html_content"`
	} `yaml:"email"`
}

type FeedSpec struct {
	Name      string   `yaml:"name"`
	URL       string   `yaml:"url"`
	parsedURL *url.URL `yaml:"-"`
}

var (
	configPath = flag.String("config", os.Getenv("FRIDER_CONFIG"), "path to config file")
	config     *Config
)

func processDomainFeeds(c chan *FeedSpec, done chan bool) {
	for f := range c {
		log.Printf("%#v", f)
	}
	close(done)
}

func processFeeds(c chan *FeedSpec, done chan bool) {
	type domainFeed struct {
		c    chan *FeedSpec
		done chan bool
	}
	domains := map[string]domainFeed{}
	for f := range c {
		dom, ok := domains[f.parsedURL.Host]
		if !ok {
			dom = domainFeed{c: make(chan *FeedSpec), done: make(chan bool)}
			domains[f.parsedURL.Host] = dom
			go processDomainFeeds(dom.c, dom.done)
		}
		dom.c <- f
	}
	for _, d := range domains {
		<-d.done
	}
	close(done)
}

func loadConfig(path string) (*Config, error) {
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
	var c Config
	if err = y.Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

func run() error {
	var err error
	if config, err = loadConfig(*configPath); err != nil {
		return fmt.Errorf("failed to load config: %s", err)
	}
	c := make(chan *FeedSpec)
	done := make(chan bool)
	go processFeeds(c, done)
	for name, f := range config.Feeds {
		u, err := url.Parse(f.URL)
		if err != nil {
			log.Printf("warn: cannot parse '%s' feed URL '%s': %s", f.name, f.URL, err)
			continue
		}
		f.parsedURL = u
		f.name = name
		c <- f
	}
	<-done
	return nil
}

func main() {
	log.SetFlags(0)
	flag.Parse()
	if err := run(); err != nil {
		log.Fatalf("error: %s", err)
	}
}
