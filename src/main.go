package git_pages

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"time"

	automemlimit "github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/c2h5oh/datasize"
	"github.com/kankanreno/go-snowflake"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var config *Config
var wildcards []*WildcardPattern
var fallback http.Handler
var backend Backend

func configureFeatures(ctx context.Context) (err error) {
	if len(config.Features) > 0 {
		logc.Println(ctx, "features:", strings.Join(config.Features, ", "))
	}
	return
}

func configureMemLimit(ctx context.Context) (err error) {
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
		logc.Println(ctx, "memlimit: now", memlimitBefore.HR())
	} else {
		logc.Println(ctx, "memlimit: was", memlimitBefore.HR(), "now", memlimitAfter.HR())
	}
	return
}

func configureWildcards(_ context.Context) (err error) {
	newWildcards, err := TranslateWildcards(config.Wildcard)
	if err != nil {
		return err
	} else {
		wildcards = newWildcards
		return nil
	}
}

func configureFallback(_ context.Context) (err error) {
	if config.Fallback.ProxyTo != nil {
		fallbackURL := &config.Fallback.ProxyTo.URL
		fallback = &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(fallbackURL)
				r.Out.Host = r.In.Host
				r.Out.Header["X-Forwarded-For"] = r.In.Header["X-Forwarded-For"]
			},
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: config.Fallback.Insecure,
				},
			},
		}
	}
	return
}

// Thread-unsafe, must be called only during initial configuration.
func configureAudit(_ context.Context) (err error) {
	snowflake.SetStartTime(time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC))
	snowflake.SetMachineID(config.Audit.NodeID)
	return
}

func listen(ctx context.Context, name string, listen string) net.Listener {
	if listen == "-" {
		return nil
	}

	protocol, address, ok := strings.Cut(listen, "/")
	if !ok {
		logc.Fatalf(ctx, "%s: %s: malformed endpoint", name, listen)
	}

	listener, err := net.Listen(protocol, address)
	if err != nil {
		logc.Fatalf(ctx, "%s: %s\n", name, err)
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

func serve(ctx context.Context, listener net.Listener, handler http.Handler) {
	if listener != nil {
		handler = panicHandler(handler)

		server := http.Server{Handler: handler}
		server.Protocols = new(http.Protocols)
		server.Protocols.SetHTTP1(true)
		if config.Feature("serve-h2c") {
			server.Protocols.SetUnencryptedHTTP2(true)
		}
		logc.Fatalln(ctx, server.Serve(listener))
	}
}

func webRootArg(arg string) string {
	switch strings.Count(arg, "/") {
	case 0:
		return arg + "/.index"
	case 1:
		return arg
	default:
		logc.Fatalln(context.Background(),
			"webroot argument must be either 'domain.tld' or 'domain.tld/dir")
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
			logc.Fatalln(context.Background(), err)
		}
	}
	return
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "(server) "+
		"git-pages [-config <file>|-no-config]\n")
	fmt.Fprintf(os.Stderr, "(admin)  "+
		"git-pages {-run-migration <name>|-freeze-domain <domain>|-unfreeze-domain <domain>}\n")
	fmt.Fprintf(os.Stderr, "(info)   "+
		"git-pages {-print-config-env-vars|-print-config}\n")
	fmt.Fprintf(os.Stderr, "(cli)    "+
		"git-pages {-get-blob|-get-manifest|-get-archive|-update-site} <ref> [file]\n")
	flag.PrintDefaults()
}

func Main() {
	ctx := context.Background()

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
	freezeDomain := flag.String("freeze-domain", "",
		"prevent any site uploads to a given `domain`")
	unfreezeDomain := flag.String("unfreeze-domain", "",
		"allow site uploads to a `domain` again after it has been frozen")
	flag.Parse()

	var cliOperations int
	if *runMigration != "" {
		cliOperations += 1
	}
	if *getBlob != "" {
		cliOperations += 1
	}
	if *getManifest != "" {
		cliOperations += 1
	}
	if *getArchive != "" {
		cliOperations += 1
	}
	if *updateSite != "" {
		cliOperations += 1
	}
	if *freezeDomain != "" {
		cliOperations += 1
	}
	if *unfreezeDomain != "" {
		cliOperations += 1
	}
	if cliOperations > 1 {
		logc.Fatalln(ctx, "-get-blob, -get-manifest, -get-archive, -update-site, -freeze, and -unfreeze are mutually exclusive")
	}

	if *configTomlPath != "" && *noConfig {
		logc.Fatalln(ctx, "-no-config and -config are mutually exclusive")
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
		logc.Fatalln(ctx, "config:", err)
	}

	if *printConfig {
		fmt.Println(config.DebugJSON())
		return
	}

	InitObservability()
	defer FiniObservability()

	if err = errors.Join(
		configureFeatures(ctx),
		configureMemLimit(ctx),
		configureWildcards(ctx),
		configureFallback(ctx),
		configureAudit(ctx),
	); err != nil {
		logc.Fatalln(ctx, err)
	}

	switch {
	case *runMigration != "":
		if backend, err = CreateBackend(&config.Storage); err != nil {
			logc.Fatalln(ctx, err)
		}

		if err := RunMigration(ctx, *runMigration); err != nil {
			logc.Fatalln(ctx, err)
		}

	case *getBlob != "":
		if backend, err = CreateBackend(&config.Storage); err != nil {
			logc.Fatalln(ctx, err)
		}

		reader, _, _, err := backend.GetBlob(ctx, *getBlob)
		if err != nil {
			logc.Fatalln(ctx, err)
		}
		io.Copy(fileOutputArg(), reader)

	case *getManifest != "":
		if backend, err = CreateBackend(&config.Storage); err != nil {
			logc.Fatalln(ctx, err)
		}

		webRoot := webRootArg(*getManifest)
		manifest, _, err := backend.GetManifest(ctx, webRoot, GetManifestOptions{})
		if err != nil {
			logc.Fatalln(ctx, err)
		}
		fmt.Fprintln(fileOutputArg(), ManifestDebugJSON(manifest))

	case *getArchive != "":
		if backend, err = CreateBackend(&config.Storage); err != nil {
			logc.Fatalln(ctx, err)
		}

		webRoot := webRootArg(*getArchive)
		manifest, metadata, err :=
			backend.GetManifest(ctx, webRoot, GetManifestOptions{})
		if err != nil {
			logc.Fatalln(ctx, err)
		}
		if err = CollectTar(ctx, fileOutputArg(), manifest, metadata); err != nil {
			logc.Fatalln(ctx, err)
		}

	case *updateSite != "":
		if backend, err = CreateBackend(&config.Storage); err != nil {
			logc.Fatalln(ctx, err)
		}

		if flag.NArg() != 1 {
			logc.Fatalln(ctx, "update source must be provided as the argument")
		}

		sourceURL, err := url.Parse(flag.Arg(0))
		if err != nil {
			logc.Fatalln(ctx, err)
		}

		var result UpdateResult
		if sourceURL.Scheme == "" {
			file, err := os.Open(sourceURL.Path)
			if err != nil {
				logc.Fatalln(ctx, err)
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
			result = UpdateFromArchive(ctx, webRoot, contentType, file)
		} else {
			branch := "pages"
			if sourceURL.Fragment != "" {
				branch, sourceURL.Fragment = sourceURL.Fragment, ""
			}

			webRoot := webRootArg(*updateSite)
			result = UpdateFromRepository(ctx, webRoot, sourceURL.String(), branch)
		}

		switch result.outcome {
		case UpdateError:
			logc.Printf(ctx, "error: %s\n", result.err)
			os.Exit(2)
		case UpdateTimeout:
			logc.Println(ctx, "timeout")
			os.Exit(1)
		case UpdateCreated:
			logc.Println(ctx, "created")
		case UpdateReplaced:
			logc.Println(ctx, "replaced")
		case UpdateDeleted:
			logc.Println(ctx, "deleted")
		case UpdateNoChange:
			logc.Println(ctx, "no-change")
		}

	case *freezeDomain != "" || *unfreezeDomain != "":
		var domain string
		var freeze bool
		if *freezeDomain != "" {
			domain = *freezeDomain
			freeze = true
		} else {
			domain = *unfreezeDomain
			freeze = false
		}

		if backend, err = CreateBackend(&config.Storage); err != nil {
			logc.Fatalln(ctx, err)
		}

		if err = backend.FreezeDomain(ctx, domain, freeze); err != nil {
			logc.Fatalln(ctx, err)
		}
		if freeze {
			log.Println("frozen")
		} else {
			log.Println("thawed")
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
				logc.Println(ctx, "config: reload err:", err)
			} else {
				// From https://go.dev/ref/mem:
				// > A read r of a memory location x holding a value that is not larger than
				// > a machine word must observe some write w such that r does not happen before
				// > w and there is no write w' such that w happens before w' and w' happens
				// > before r. That is, each read must observe a value written by a preceding or
				// > concurrent write.
				config = newConfig
				if err = errors.Join(
					configureFeatures(ctx),
					configureMemLimit(ctx),
					configureWildcards(ctx),
					configureFallback(ctx),
				); err != nil {
					// At this point the configuration is in an in-between, corrupted state, so
					// the only reasonable choice is to crash.
					logc.Fatalln(ctx, "config: reload fail:", err)
				} else {
					logc.Println(ctx, "config: reload ok")
				}
			}
		})

		// Start listening on all ports before initializing the backend, otherwise if the backend
		// spends some time initializing (which the S3 backend does) a proxy like Caddy can race
		// with git-pages on startup and return errors for requests that would have been served
		// just 0.5s later.
		pagesListener := listen(ctx, "pages", config.Server.Pages)
		caddyListener := listen(ctx, "caddy", config.Server.Caddy)
		metricsListener := listen(ctx, "metrics", config.Server.Metrics)

		if backend, err = CreateBackend(&config.Storage); err != nil {
			logc.Fatalln(ctx, err)
		}
		backend = NewObservedBackend(backend)

		go serve(ctx, pagesListener, ObserveHTTPHandler(http.HandlerFunc(ServePages)))
		go serve(ctx, caddyListener, ObserveHTTPHandler(http.HandlerFunc(ServeCaddy)))
		go serve(ctx, metricsListener, promhttp.Handler())

		if config.Insecure {
			logc.Println(ctx, "serve: ready (INSECURE)")
		} else {
			logc.Println(ctx, "serve: ready")
		}

		WaitForInterrupt()
		logc.Println(ctx, "serve: exiting")
	}
}
