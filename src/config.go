package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/c2h5oh/datasize"
	"github.com/creasty/defaults"
	"github.com/pelletier/go-toml/v2"
)

// For some reason, the standard `time.Duration` type doesn't implement the standard
// `encoding.{TextMarshaler,TextUnmarshaler}` interfaces.
type Duration time.Duration

func (t Duration) String() string {
	return fmt.Sprint(time.Duration(t))
}

func (t *Duration) UnmarshalText(data []byte) (err error) {
	u, err := time.ParseDuration(string(data))
	*t = Duration(u)
	return
}

func (t *Duration) MarshalText() ([]byte, error) {
	return []byte(t.String()), nil
}

type Config struct {
	Insecure  bool             `toml:"-" env:"insecure"`
	Features  []string         `toml:"features"`
	LogFormat string           `toml:"log-format" default:"datetime+message"`
	Server    ServerConfig     `toml:"server"`
	Wildcard  []WildcardConfig `toml:"wildcard"`
	Storage   StorageConfig    `toml:"storage"`
	Limits    LimitsConfig     `toml:"limits"`
}

type ServerConfig struct {
	Pages   string `toml:"pages" default:"tcp/:3000"`
	Caddy   string `toml:"caddy" default:"tcp/:3001"`
	Health  string `toml:"health" default:"tcp/:3002"`
	Metrics string `toml:"metrics" default:"tcp/:3003"`
}

type WildcardConfig struct {
	Domain          string   `toml:"domain"`
	CloneURL        string   `toml:"clone-url"`
	IndexRepos      []string `toml:"index-repos" default:"[]"`
	FallbackProxyTo string   `toml:"fallback-proxy-to"`
}

type CacheConfig struct {
	MaxSize datasize.ByteSize `toml:"max-size"`
	MaxAge  Duration          `toml:"max-age"`
}

type StorageConfig struct {
	Type string   `toml:"type" default:"fs"`
	FS   FSConfig `toml:"fs"  default:"{\"Root\":\"./data\"}"`
	S3   S3Config `toml:"s3"`
}

type FSConfig struct {
	Root string `toml:"root"`
}

type S3Config struct {
	Endpoint        string      `toml:"endpoint"`
	Insecure        bool        `toml:"insecure"`
	AccessKeyID     string      `toml:"access-key-id"`
	SecretAccessKey string      `toml:"secret-access-key"`
	Region          string      `toml:"region"`
	Bucket          string      `toml:"bucket"`
	BlobCache       CacheConfig `toml:"blob-cache" default:"{\"MaxSize\":\"256MB\"}"`
	SiteCache       CacheConfig `toml:"site-cache" default:"{\"MaxAge\":\"60s\",\"MaxSize\":\"16MB\"}"`
}

type LimitsConfig struct {
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
	UpdateTimeout Duration `toml:"update-timeout" default:"60s"`
	// Soft limit on Go heap size, expressed as a fraction of total available RAM.
	MaxHeapSizeRatio float64 `toml:"max-heap-size-ratio" default:"0.5"`
}

func (config *Config) DebugJSON() string {
	result, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(result)
}

func (config *Config) Feature(name string) bool {
	return slices.Contains(config.Features, name)
}

type walkConfigState struct {
	config    reflect.Value
	scopeType reflect.Type
	index     []int
	segments  []string
}

func walkConfigScope(scopeState walkConfigState, onKey func(string, reflect.Value) error) (err error) {
	for _, field := range reflect.VisibleFields(scopeState.scopeType) {
		fieldState := walkConfigState{config: scopeState.config}
		fieldState.scopeType = field.Type
		fieldState.index = append(scopeState.index, field.Index...)
		var tagValue, ok = "", false
		if tagValue, ok = field.Tag.Lookup("env"); !ok {
			if tagValue, ok = field.Tag.Lookup("toml"); !ok {
				continue // implicit skip
			}
		} else if tagValue == "-" {
			continue // explicit skip
		}
		fieldSegment := strings.ReplaceAll(strings.ToUpper(tagValue), "-", "_")
		fieldState.segments = append(scopeState.segments, fieldSegment)
		switch field.Type.Kind() {
		case reflect.Struct:
			err = walkConfigScope(fieldState, onKey)
		default:
			err = onKey(
				strings.Join(fieldState.segments, "_"),
				scopeState.config.FieldByIndex(fieldState.index),
			)
		}
		if err != nil {
			return
		}
	}
	return
}

func walkConfig(config *Config, onKey func(string, reflect.Value) error) error {
	state := walkConfigState{
		config:    reflect.ValueOf(config).Elem(),
		scopeType: reflect.TypeOf(config).Elem(),
		index:     []int{},
		segments:  []string{"PAGES"},
	}
	return walkConfigScope(state, onKey)
}

func setConfigValue(reflValue reflect.Value, repr string) (err error) {
	valueAny := reflValue.Interface()
	switch valueCast := valueAny.(type) {
	case string:
		reflValue.SetString(repr)
	case []string:
		reflValue.Set(reflect.ValueOf(strings.Split(repr, ",")))
	case bool:
		if valueCast, err = strconv.ParseBool(repr); err == nil {
			reflValue.SetBool(valueCast)
		}
	case uint:
		var parsed uint64
		if parsed, err = strconv.ParseUint(repr, 10, strconv.IntSize); err == nil {
			reflValue.SetUint(parsed)
		}
	case float64:
		if valueCast, err = strconv.ParseFloat(repr, 64); err == nil {
			reflValue.SetFloat(valueCast)
		}
	case datasize.ByteSize:
		if valueCast, err = datasize.ParseString(repr); err == nil {
			reflValue.Set(reflect.ValueOf(valueCast))
		}
	case time.Duration:
		if valueCast, err = time.ParseDuration(repr); err == nil {
			reflValue.Set(reflect.ValueOf(valueCast))
		}
	case Duration:
		var parsed time.Duration
		if parsed, err = time.ParseDuration(repr); err == nil {
			reflValue.Set(reflect.ValueOf(Duration(parsed)))
		}
	case []WildcardConfig:
		var parsed []*WildcardConfig
		decoder := json.NewDecoder(bytes.NewReader([]byte(repr)))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&parsed); err == nil {
			var assigned []WildcardConfig
			for _, wildcard := range parsed {
				defaults.MustSet(wildcard)
				assigned = append(assigned, *wildcard)
			}
			reflValue.Set(reflect.ValueOf(assigned))
		}
	default:
		panic("unhandled config value type")
	}
	return err
}

func PrintConfigEnvVars() {
	config := Config{}
	defaults.MustSet(&config)

	walkConfig(&config, func(envName string, reflValue reflect.Value) (err error) {
		value := reflValue.Interface()
		reprBefore := fmt.Sprint(value)
		fmt.Printf("%s %T = %q\n", envName, value, reprBefore)
		// make sure that the value, at least, roundtrips
		setConfigValue(reflValue, reprBefore)
		reprAfter := fmt.Sprint(value)
		if reprBefore != reprAfter {
			panic("failed to roundtrip config value")
		}
		return
	})
}

func Configure(tomlPath string) (config *Config, err error) {
	// start with an all-default configuration
	config = new(Config)
	defaults.MustSet(config)

	// inject values from `config.toml`
	if tomlPath != "" {
		var file *os.File
		file, err = os.Open(tomlPath)
		if err != nil {
			return
		}
		defer file.Close()

		decoder := toml.NewDecoder(file)
		decoder.DisallowUnknownFields()
		decoder.EnableUnmarshalerInterface()
		if err = decoder.Decode(&config); err != nil {
			return
		}
	}

	// inject values from the environment, overriding everything else
	err = walkConfig(config, func(envName string, reflValue reflect.Value) error {
		if envValue, found := os.LookupEnv(envName); found {
			return setConfigValue(reflValue, envValue)
		}
		return nil
	})

	return
}
