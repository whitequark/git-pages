package main

import (
	"flag"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/KimMachineGun/automemlimit/memlimit"
)

var backend Backend

func listen(name string, listen string) net.Listener {
	if listen == "" {
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

func serve(listener net.Listener, serve func(http.ResponseWriter, *http.Request)) {
	if listener != nil {
		if err := http.Serve(listener, http.HandlerFunc(serve)); err != nil {
			log.Fatalln(err)
		}
	}
}

func main() {
	var err error

	configPath := flag.String("config", "config.toml", "path to configuration file")
	migrateV1Path := flag.String("migrate-v1", "", "path to v1 data directory to upload")
	flag.Parse()

	if err := ReadConfig(*configPath); err != nil {
		log.Fatalln("config:", err)
	}
	UpdateConfigEnv() // environment takes priority
	CompileWildcardPattern()

	switch config.LogFormat {
	case "short":
		log.SetFlags(0)
	default:
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
		memlimit.WithRatio(0.9),
	)

	// Start listening on all ports before initializing the backend, otherwise if the backend
	// spends some time initializing (which the S3 backend does) a proxy like Caddy can race
	// with git-pages on startup and return errors for requests that would have been served
	// just 0.5s later.
	pagesListener := listen("pages", config.Listen.Pages)
	caddyListener := listen("caddy", config.Listen.Caddy)
	healthListener := listen("health", config.Listen.Health)

	switch config.Backend.Type {
	case "fs":
		if backend, err = NewFSBackend(config.Backend.FS.Root); err != nil {
			log.Fatalln("fs backend:", err)
		}

	case "s3":
		if backend, err = NewS3Backend(
			config.Backend.S3.Endpoint,
			config.Backend.S3.Insecure,
			config.Backend.S3.AccessKeyID,
			config.Backend.S3.SecretAccessKey,
			config.Backend.S3.Region,
			config.Backend.S3.Bucket,
		); err != nil {
			log.Fatalln("s3 backend:", err)
		}

	default:
		log.Fatalln("unknown backend:", config.Backend.Type)
	}

	if *migrateV1Path != "" {
		root, err := os.OpenRoot(*migrateV1Path)
		if err != nil {
			log.Fatalln("migrate v1:", err)
		}

		err = MigrateFromV1(root)
		if err != nil {
			log.Fatalln("migrate v1:", err)
		}

		log.Println("migrate v1 ok")
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
