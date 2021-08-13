package main

import (
	"flag"

	"fmt"
	"log"
	"os"

	"github.com/gocolly/colly/v2"
	_ "github.com/gorilla/feeds"
	_ "github.com/joho/godotenv/autoload"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Feeds map[string]FeedSpec `yaml:"feeds"`
}

type FeedSpec struct {
	URL string `yaml:"url"`
}

var (
	configPath = flag.String("config", os.Getenv("FRIDER_CONFIG"), "path to config file")
	config     *Config
)

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
	c := colly.NewCollector()
	c.OnResponse(func(r *colly.Response) {
		log.Printf("ddd")
	})
	c.Wait()
	return nil
}

func main() {
	log.SetFlags(0)
	flag.Parse()
	if err := run(); err != nil {
		log.Fatalf("error: %s", err)
	}
}
