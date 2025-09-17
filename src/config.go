package main

import (
	"os"

	"github.com/pelletier/go-toml/v2"
)

type ListenConfig struct {
	Protocol string `toml:"protocol"`
	Address  string `toml:"address"`
}

type Config struct {
	Pages    ListenConfig `toml:"pages"`
	Caddy    ListenConfig `toml:"caddy"`
	Wildcard struct {
		Domain    string `toml:"domain"`
		CloneURL  string `toml:"clone-url"`
		IndexRepo string `toml:"index-repo"`
	} `toml:"wildcard"`
	Backend struct {
		Type string `toml:"type"`
		FS   struct {
			Root string `toml:"root"`
		} `toml:"fs"`
		S3 struct {
			Endpoint        string `toml:"endpoint"`
			Insecure        bool   `toml:"insecure"`
			AccessKeyID     string `toml:"access-key-id"`
			SecretAccessKey string `toml:"secret-access-key"`
			Region          string `toml:"region"`
			Bucket          string `toml:"bucket"`
			Cache           struct {
				MaxSize uint64 `toml:"max-size"` // in bytes
				MaxAge  string `toml:"max-age"`
			} `toml:"cache"`
		}
	} `toml:"backend"`
}

func ReadConfig(path string, config *Config) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := toml.NewDecoder(file)
	decoder.DisallowUnknownFields()
	return decoder.Decode(config)
}
