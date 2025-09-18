package main

import (
	"log"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/valyala/fasttemplate"
)

type CacheConfig struct {
	MaxSize uint64 `toml:"max-size"` // in bytes
	MaxAge  string `toml:"max-age"`
}

type Config struct {
	LogFormat string `toml:"log-format"`
	Listen    struct {
		Pages  string `toml:"pages"`
		Caddy  string `toml:"caddy"`
		Health string `toml:"health"`
	} `toml:"listen"`
	Wildcard struct {
		Domain     string   `toml:"domain"`
		CloneURL   string   `toml:"clone-url"`
		IndexRepos []string `toml:"index-repos"`
	} `toml:"wildcard"`
	Backend struct {
		Type string `toml:"type"`
		FS   struct {
			Root string `toml:"root"`
		} `toml:"fs"`
		S3 struct {
			Endpoint        string      `toml:"endpoint"`
			Insecure        bool        `toml:"insecure"`
			AccessKeyID     string      `toml:"access-key-id"`
			SecretAccessKey string      `toml:"secret-access-key"`
			Region          string      `toml:"region"`
			Bucket          string      `toml:"bucket"`
			BlobCache       CacheConfig `toml:"blob-cache"`
			SiteCache       CacheConfig `toml:"site-cache"`
		}
	} `toml:"backend"`
}

type WildcardPattern struct {
	Domain     []string
	CloneURL   *fasttemplate.Template
	IndexRepos []*fasttemplate.Template
}

var config Config
var wildcardPattern WildcardPattern

func ReadConfig(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := toml.NewDecoder(file)
	decoder.DisallowUnknownFields()
	return decoder.Decode(&config)
}

func updateFromEnv(dest *string, key string) {
	if value, found := os.LookupEnv(key); found {
		*dest = value
	}
}

func UpdateConfigEnv() {
	updateFromEnv(&config.Backend.Type, "BACKEND")
	updateFromEnv(&config.Backend.FS.Root, "FS_ROOT")
	updateFromEnv(&config.Backend.S3.Endpoint, "S3_ENDPOINT")
	updateFromEnv(&config.Backend.S3.AccessKeyID, "S3_ACCESS_KEY_ID")
	updateFromEnv(&config.Backend.S3.SecretAccessKey, "S3_SECRET_ACCESS_KEY")
	updateFromEnv(&config.Backend.S3.Region, "S3_REGION")
	updateFromEnv(&config.Backend.S3.Bucket, "S3_BUCKET")
}

func CompileWildcardPattern() {
	wildcardPattern = WildcardPattern{
		Domain: strings.Split(config.Wildcard.Domain, "."),
	}

	template, err := fasttemplate.NewTemplate(config.Wildcard.CloneURL, "<", ">")
	if err != nil {
		log.Fatalf("wildcard pattern: clone URL: %s", err)
	} else {
		wildcardPattern.CloneURL = template
	}

	for _, indexRepo := range config.Wildcard.IndexRepos {
		template, err := fasttemplate.NewTemplate(indexRepo, "<", ">")
		if err != nil {
			log.Fatalf("wildcard pattern: clone URL: %s", err)
		} else {
			wildcardPattern.IndexRepos = append(wildcardPattern.IndexRepos, template)
		}
	}
}
