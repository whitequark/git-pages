package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/KimMachineGun/automemlimit/memlimit"
)

var features []string

func FeatureActive(feature string) bool {
	if features == nil {
		features = strings.Split(strings.ToLower(os.Getenv("FEATURES")), ",")
	}
	return slices.Contains(features, strings.ToLower(feature))
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

func serve(listener net.Listener, serve func(http.ResponseWriter, *http.Request)) {
	if listener != nil {
		var handler http.Handler
		handler = http.HandlerFunc(serve)
		handler = ObserveHTTPHandler(handler)
		handler = panicHandler(handler)

		server := http.Server{Handler: handler}
		server.Protocols = new(http.Protocols)
		server.Protocols.SetHTTP1(true)
		if FeatureActive("h2c") {
			server.Protocols.SetUnencryptedHTTP2(true)
		}
		log.Fatalln(server.Serve(listener))
	}
}

func main() {
	InitObservability()

	configPath := flag.String("config", "config.toml",
		"path to configuration file")
	checkConfig := flag.Bool("check-config", false,
		"validate configuration, print it as JSON, and exit")
	getManifest := flag.String("get-manifest", "",
		"retrieve manifest for web root as ProtoJSON")
	flag.Parse()

	if err := ReadConfig(*configPath); err != nil {
		log.Fatalln("config:", err)
	}
	UpdateConfigEnv() // environment takes priority

	if *checkConfig {
		configJSON, _ := json.MarshalIndent(&config, "", "  ")
		fmt.Println(string(configJSON))
		return
	}

	switch config.LogFormat {
	case "message":
		log.SetFlags(0)
	case "datetime+message":
		log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	}

	// Avoid being OOM killed by not garbage collecting early enough.
	memlimit.SetGoMemLimitWithOpts(
		memlimit.WithLogger(slog.Default()),
		memlimit.WithProvider(
			memlimit.ApplyFallback(
				memlimit.FromCgroup,
				memlimit.FromSystem,
			),
		),
		memlimit.WithRatio(float64(config.Limits.MaxHeapSizeRatio)),
	)

	if *getManifest != "" {
		if err := ConfigureBackend(); err != nil {
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
	} else {
		// Start listening on all ports before initializing the backend, otherwise if the backend
		// spends some time initializing (which the S3 backend does) a proxy like Caddy can race
		// with git-pages on startup and return errors for requests that would have been served
		// just 0.5s later.
		pagesListener := listen("pages", config.Listen.Pages)
		caddyListener := listen("caddy", config.Listen.Caddy)
		healthListener := listen("health", config.Listen.Health)

		if err := ConfigureBackend(); err != nil {
			log.Fatalln(err)
		}

		if err := ConfigureWildcards(); err != nil {
			log.Fatalln(err)
		}

		go serve(pagesListener, ServePages)
		go serve(caddyListener, ServeCaddy)
		go serve(healthListener, ServeHealth)

		if InsecureMode() {
			log.Println("ready (INSECURE)")
		} else {
			log.Println("ready")
		}
		select {}
	}
}
