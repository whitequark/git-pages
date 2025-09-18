package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
)

var backend Backend

func serveHandler(name string, listen string, serve func(http.ResponseWriter, *http.Request)) {
	protocol, address, ok := strings.Cut(listen, "/")
	if !ok {
		log.Fatalf("%s: %s: malformed endpoint", name, listen)
	}

	listener, err := net.Listen(protocol, address)
	if err != nil {
		log.Fatalf("%s: %s\n", name, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", serve)
	if err := http.Serve(listener, mux); err != nil {
		log.Fatalf("%s: %s\n", name, err)
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

	switch config.LogFormat {
	case "short":
		log.SetFlags(0)
	default:
		log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	}

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

	log.Println("ready")

	go serveHandler("pages", config.Listen.Pages, ServePages)

	if config.Listen.Caddy != "" {
		go serveHandler("caddy", config.Listen.Caddy, ServeCaddy)
	}

	if config.Listen.Health != "" {
		go serveHandler("health", config.Listen.Health, ServeHealth)
	}

	select {}
}
