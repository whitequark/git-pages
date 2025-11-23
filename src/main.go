package git_pages

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strings"

	automemlimit "github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/c2h5oh/datasize"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var config *Config
var wildcards []*WildcardPattern
var backend Backend

func configureFeatures() (err error) {
	if len(config.Features) > 0 {
		log.Println("features:", strings.Join(config.Features, ", "))
	}
	return
}

func configureMemLimit() (err error) {
	// Avoid being OOM killed by not garbage collecting early enough.
	memlimitBefore := datasize.ByteSize(debug.SetMemoryLimit(-1))
	automemlimit.SetGoMemLimitWithOpts(
		automemlimit.WithLogger(slog.New(slog.DiscardHandler)),
		automemlimit.WithProvider(
			automemlimit.ApplyFallback(
				automemlimit.FromCgroup,
				automemlimit.FromSystem,
			),
		),
		automemlimit.WithRatio(float64(config.Limits.MaxHeapSizeRatio)),
	)
	memlimitAfter := datasize.ByteSize(debug.SetMemoryLimit(-1))
	if memlimitBefore == memlimitAfter {
		log.Println("memlimit: now", memlimitBefore.HR())
	} else {
		log.Println("memlimit: was", memlimitBefore.HR(), "now", memlimitAfter.HR())
	}
	return
}

func configureWildcards() (err error) {
	newWildcards, err := TranslateWildcards(config.Wildcard)
	if err != nil {
		return err
	} else {
		wildcards = newWildcards
		return nil
	}
}

func listen(name string, listen string) net.Listener {
	if listen == "-" {
		return nil
	}

	protocol, address, ok := strings.Cut(listen, "/")
	if !ok {
		log.Fatalf("%s: %s: malformed endpoint", name, listen)
	}

	listener, err := net.Listen(protocol, address)
	if err != nil {
		log.Fatalf("%s: %s\n", name, err)
	}

	return listener
}

func panicHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				logc.Printf(r.Context(), "panic: %s %s %s: %s\n%s",
					r.Method, r.Host, r.URL.Path, err, string(debug.Stack()))
				http.Error(w,
					fmt.Sprintf("internal server error: %s", err),
					http.StatusInternalServerError,
				)
			}
		}()
		handler.ServeHTTP(w, r)
	})
}

func serve(listener net.Listener, handler http.Handler) {
	if listener != nil {
		handler = panicHandler(handler)

		server := http.Server{Handler: handler}
		server.Protocols = new(http.Protocols)
		server.Protocols.SetHTTP1(true)
		if config.Feature("serve-h2c") {
			server.Protocols.SetUnencryptedHTTP2(true)
		}
		log.Fatalln(server.Serve(listener))
	}
}

func webRootArg(arg string) string {
	switch strings.Count(arg, "/") {
	case 0:
		return arg + "/.index"
	case 1:
		return arg
	default:
		log.Fatalf("webroot argument must be either 'domain.tld' or 'domain.tld/dir")
		return ""
	}
}

func fileOutputArg() (writer io.WriteCloser) {
	var err error
	if flag.NArg() == 0 {
		writer = os.Stdout
	} else {
		writer, err = os.Create(flag.Arg(0))
		if err != nil {
			log.Fatalln(err)
		}
	}
	return
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "(server) "+
		"git-pages [-config <file>|-no-config]\n")
	fmt.Fprintf(os.Stderr, "(admin)  "+
		"git-pages {-run-migration <name>}\n")
	fmt.Fprintf(os.Stderr, "(info)   "+
		"git-pages {-print-config-env-vars|-print-config}\n")
	fmt.Fprintf(os.Stderr, "(cli)    "+
		"git-pages {-get-blob|-get-manifest|-get-archive|-update-site} <ref> [file]\n")
	flag.PrintDefaults()
}

func Main() {
	flag.Usage = usage
	printConfigEnvVars := flag.Bool("print-config-env-vars", false,
		"print every recognized configuration environment variable and exit")
	printConfig := flag.Bool("print-config", false,
		"print configuration as JSON and exit")
	configTomlPath := flag.String("config", "",
		"load configuration from `filename` (default: 'config.toml')")
	noConfig := flag.Bool("no-config", false,
		"run without configuration file (configure via environment variables)")
	runMigration := flag.String("run-migration", "",
		"run a store `migration` (one of: create-domain-markers)")
	getBlob := flag.String("get-blob", "",
		"write contents of `blob` ('sha256-xxxxxxx...xxx')")
	getManifest := flag.String("get-manifest", "",
		"write manifest for `site` (either 'domain.tld' or 'domain.tld/dir') as ProtoJSON")
	getArchive := flag.String("get-archive", "",
		"write archive for `site` (either 'domain.tld' or 'domain.tld/dir') in tar format")
	updateSite := flag.String("update-site", "",
		"update `site` (either 'domain.tld' or 'domain.tld/dir') from archive or repository URL")
	flag.Parse()

	var cliOperations int
	if *getBlob != "" {
		cliOperations += 1
	}
	if *getManifest != "" {
		cliOperations += 1
	}
	if *getArchive != "" {
		cliOperations += 1
	}
	if cliOperations > 1 {
		log.Fatalln("-get-blob, -get-manifest, and -get-archive are mutually exclusive")
	}

	if *configTomlPath != "" && *noConfig {
		log.Fatalln("-no-config and -config are mutually exclusive")
	}

	if *printConfigEnvVars {
		PrintConfigEnvVars()
		return
	}

	var err error
	if *configTomlPath == "" && !*noConfig {
		*configTomlPath = "config.toml"
	}
	if config, err = Configure(*configTomlPath); err != nil {
		log.Fatalln("config:", err)
	}

	if *printConfig {
		fmt.Println(config.DebugJSON())
		return
	}

	InitObservability()
	defer FiniObservability()

	if err = errors.Join(
		configureFeatures(),
		configureMemLimit(),
		configureWildcards(),
	); err != nil {
		log.Fatalln(err)
	}

	switch {
	case *runMigration != "":
		if backend, err = CreateBackend(&config.Storage); err != nil {
			log.Fatalln(err)
		}

		if err := RunMigration(context.Background(), *runMigration); err != nil {
			log.Fatalln(err)
		}

	case *getBlob != "":
		if backend, err = CreateBackend(&config.Storage); err != nil {
			log.Fatalln(err)
		}

		reader, _, _, err := backend.GetBlob(context.Background(), *getBlob)
		if err != nil {
			log.Fatalln(err)
		}
		io.Copy(fileOutputArg(), reader)

	case *getManifest != "":
		if backend, err = CreateBackend(&config.Storage); err != nil {
			log.Fatalln(err)
		}

		webRoot := webRootArg(*getManifest)
		manifest, _, err := backend.GetManifest(context.Background(), webRoot, GetManifestOptions{})
		if err != nil {
			log.Fatalln(err)
		}
		fmt.Fprintln(fileOutputArg(), ManifestDebugJSON(manifest))

	case *getArchive != "":
		if backend, err = CreateBackend(&config.Storage); err != nil {
			log.Fatalln(err)
		}

		webRoot := webRootArg(*getArchive)
		manifest, manifestMtime, err :=
			backend.GetManifest(context.Background(), webRoot, GetManifestOptions{})
		if err != nil {
			log.Fatalln(err)
		}
		CollectTar(context.Background(), fileOutputArg(), manifest, manifestMtime)

	case *updateSite != "":
		if backend, err = CreateBackend(&config.Storage); err != nil {
			log.Fatalln(err)
		}

		if flag.NArg() != 1 {
			log.Fatalln("update source must be provided as the argument")
		}

		sourceURL, err := url.Parse(flag.Arg(0))
		if err != nil {
			log.Fatalln(err)
		}

		var result UpdateResult
		if sourceURL.Scheme == "" {
			file, err := os.Open(sourceURL.Path)
			if err != nil {
				log.Fatalln(err)
			}
			defer file.Close()

			var contentType string
			switch {
			case strings.HasSuffix(sourceURL.Path, ".zip"):
				contentType = "application/zip"
			case strings.HasSuffix(sourceURL.Path, ".tar"):
				contentType = "application/x-tar"
			case strings.HasSuffix(sourceURL.Path, ".tar.gz"):
				contentType = "application/x-tar+gzip"
			case strings.HasSuffix(sourceURL.Path, ".tar.zst"):
				contentType = "application/x-tar+zstd"
			default:
				log.Fatalf("cannot determine content type from filename %q\n", sourceURL)
			}

			webRoot := webRootArg(*updateSite)
			result = UpdateFromArchive(context.Background(), webRoot, contentType, file)
		} else {
			branch := "pages"
			if sourceURL.Fragment != "" {
				branch, sourceURL.Fragment = sourceURL.Fragment, ""
			}

			webRoot := webRootArg(*updateSite)
			result = UpdateFromRepository(context.Background(), webRoot, sourceURL.String(), branch)
		}

		switch result.outcome {
		case UpdateError:
			log.Printf("error: %s\n", result.err)
			os.Exit(2)
		case UpdateTimeout:
			log.Println("timeout")
			os.Exit(1)
		case UpdateCreated:
			log.Println("created")
		case UpdateReplaced:
			log.Println("replaced")
		case UpdateDeleted:
			log.Println("deleted")
		case UpdateNoChange:
			log.Println("no-change")
		}

	default:
		// Hook a signal (SIGHUP on *nix, nothing on Windows) for reloading the configuration
		// at runtime. This is useful because it preserves S3 backend cache contents. Failed
		// configuration reloads will not crash the process; you may want to check the syntax
		// first with `git-pages -config ... -print-config` since there is no other feedback.
		//
		// Note that not all of the configuration is updated on reload. Listeners are kept as-is.
		// The backend is not recreated (this is intentional as it allows preserving the cache).
		OnReload(func() {
			if newConfig, err := Configure(*configTomlPath); err != nil {
				log.Println("config: reload err:", err)
			} else {
				// From https://go.dev/ref/mem:
				// > A read r of a memory location x holding a value that is not larger than
				// > a machine word must observe some write w such that r does not happen before
				// > w and there is no write w' such that w happens before w' and w' happens
				// > before r. That is, each read must observe a value written by a preceding or
				// > concurrent write.
				config = newConfig
				if err = errors.Join(
					configureFeatures(),
					configureMemLimit(),
					configureWildcards(),
				); err != nil {
					// At this point the configuration is in an in-between, corrupted state, so
					// the only reasonable choice is to crash.
					log.Fatalln("config: reload fail:", err)
				} else {
					log.Println("config: reload ok")
				}
			}
		})

		// Start listening on all ports before initializing the backend, otherwise if the backend
		// spends some time initializing (which the S3 backend does) a proxy like Caddy can race
		// with git-pages on startup and return errors for requests that would have been served
		// just 0.5s later.
		pagesListener := listen("pages", config.Server.Pages)
		caddyListener := listen("caddy", config.Server.Caddy)
		metricsListener := listen("metrics", config.Server.Metrics)

		if backend, err = CreateBackend(&config.Storage); err != nil {
			log.Fatalln(err)
		}
		backend = NewObservedBackend(backend)

		go serve(pagesListener, ObserveHTTPHandler(http.HandlerFunc(ServePages)))
		go serve(caddyListener, ObserveHTTPHandler(http.HandlerFunc(ServeCaddy)))
		go serve(metricsListener, promhttp.Handler())

		if config.Insecure {
			log.Println("serve: ready (INSECURE)")
		} else {
			log.Println("serve: ready")
		}

		WaitForInterrupt()
		log.Println("serve: exiting")
	}
}
