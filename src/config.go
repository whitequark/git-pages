package main

import (
	"os"
	"time"

	"github.com/c2h5oh/datasize"
	"github.com/creasty/defaults"
	"github.com/pelletier/go-toml/v2"
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
	Wildcard []struct {
		Domain          string   `toml:"domain"`
		CloneURL        string   `toml:"clone-url"`
		IndexRepos      []string `toml:"index-repos"`
		FallbackProxyTo string   `toml:"fallback-proxy-to"`
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
	Limits struct {
		// Maximum size of a single published site. Also used to limit the size of archive
		// uploads and other similar overconsumption conditions.
		MaxSiteSize datasize.ByteSize `toml:"max-site-size" default:"128M"`
		// Maximum size of a single site manifest, computed over its binary Protobuf
		// serialization.
		MaxManifestSize datasize.ByteSize `toml:"max-manifest-size" default:"1M"`
		// Maximum size of a file that will still be inlined into the site manifest.
		MaxInlineFileSize datasize.ByteSize `toml:"max-inline-file-size" default:"256B"`
		// Maximum size of a Git object that will be cached in memory during Git operations.
		GitLargeObjectThreshold datasize.ByteSize `toml:"git-large-object-threshold" default:"1M"`
		// Maximum number of symbolic link traversals before the path is considered unreachable.
		MaxSymlinkDepth uint `toml:"max-symlink-depth" default:"16"`
		// Maximum time that an update operation (PUT or POST request) could take before being
		// interrupted.
		UpdateTimeout time.Duration `toml:"update-timeout" default:"60s"`
		// Soft limit on Go heap size, expressed as a fraction of total available RAM.
		MaxHeapSizeRatio float64 `toml:"max-heap-size-ratio" default:"0.5"`
	} `toml:"limits"`
}

var config Config

func ReadConfig(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := toml.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return err
	}

	defaults.MustSet(&config)

	return nil
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
