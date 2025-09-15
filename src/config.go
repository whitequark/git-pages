package main

import (
	"os"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	DataDir string `toml:"data-dir"`
	Listen  struct {
		Protocol string `toml:"protocol"`
		Address  string `toml:"address"`
	} `toml:"listen"`
	Wildcard struct {
		Domain    string `toml:"domain"`
		CloneURL  string `toml:"clone-url"`
		IndexRepo string `toml:"index-repo"`
	} `toml:"wildcard"`
}

func readConfig(path string, config *Config) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := toml.NewDecoder(file)
	decoder.DisallowUnknownFields()
	return decoder.Decode(config)
}
