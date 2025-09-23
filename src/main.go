package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"

	automemlimit "github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/c2h5oh/datasize"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var config *Config

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
				log.Printf("panic: %s %s %s: %s\n%s",
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
		handler = ObserveHTTPHandler(handler)
		handler = panicHandler(handler)

		server := http.Server{Handler: handler}
		server.Protocols = new(http.Protocols)
		server.Protocols.SetHTTP1(true)
		if config.Feature("h2c") {
			server.Protocols.SetUnencryptedHTTP2(true)
		}
		log.Fatalln(server.Serve(listener))
	}
}

func main() {
	InitObservability()

	printConfigEnvVars := flag.Bool("print-config-env-vars", false,
		"print every recognized configuration environment variable and exit")
	printConfig := flag.Bool("print-config", false,
		"print configuration as JSON and exit")
	configTomlPath := flag.String("config", "config.toml",
		"load configuration from `filename`")
	getManifest := flag.String("get-manifest", "",
		"write manifest for `webroot` (either 'domain.tld' or 'domain.tld/dir') to stdout as ProtoJSON")
	getBlob := flag.String("get-blob", "",
		"write `blob` ('sha256-xxxxxxx...xxx') to stdout")
	flag.Parse()

	if *getManifest != "" && *getBlob != "" {
		log.Fatalln("-get-manifest and -get-blob are mutually exclusive")
	}

	if *printConfigEnvVars {
		PrintConfigEnvVars()
		return
	}

	var err error
	if config, err = Configure(*configTomlPath); err != nil {
		log.Fatalln("config:", err)
	}

	if *printConfig {
		fmt.Println(config.DebugJSON())
		return
	}

	switch config.LogFormat {
	case "message":
		log.SetFlags(0)
	case "datetime+message":
		log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	}

	if len(config.Features) > 0 {
		log.Println("features:", strings.Join(config.Features, ", "))
	}

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

	switch {
	case *getManifest != "":
		if err := ConfigureBackend(&config.Storage); err != nil {
			log.Fatalln(err)
		}

		webRoot := *getManifest
		if !strings.Contains(webRoot, "/") {
			webRoot += "/.index"
		}

		manifest, err := backend.GetManifest(webRoot)
		if err != nil {
			log.Fatalln(err)
		}
		fmt.Println(ManifestDebugJSON(manifest))

	case *getBlob != "":
		if err := ConfigureBackend(&config.Storage); err != nil {
			log.Fatalln(err)
		}

		reader, _, err := backend.GetBlob(*getBlob)
		if err != nil {
			log.Fatalln(err)
		}

		io.Copy(os.Stdout, reader)

	default:
		// Start listening on all ports before initializing the backend, otherwise if the backend
		// spends some time initializing (which the S3 backend does) a proxy like Caddy can race
		// with git-pages on startup and return errors for requests that would have been served
		// just 0.5s later.
		pagesListener := listen("pages", config.Server.Pages)
		caddyListener := listen("caddy", config.Server.Caddy)
		metricsListener := listen("metrics", config.Server.Metrics)

		if err := ConfigureBackend(&config.Storage); err != nil {
			log.Fatalln(err)
		}

		if err := ConfigureWildcards(config.Wildcard); err != nil {
			log.Fatalln(err)
		}

		go serve(pagesListener, http.HandlerFunc(ServePages))
		go serve(caddyListener, http.HandlerFunc(ServeCaddy))
		go serve(metricsListener, promhttp.Handler())

		if config.Insecure {
			log.Println("serve: ready (INSECURE)")
		} else {
			log.Println("serve: ready")
		}
		select {}
	}
}
