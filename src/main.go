package main

import (
	"flag"
	"log"
	"net"
	"net/http"
)

var backend Backend

func serveHandler(name string, listen ListenConfig, serve func(http.ResponseWriter, *http.Request)) {
	listener, err := net.Listen(listen.Protocol, listen.Address)
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
	flag.Parse()

	if err := ReadConfig(*configPath); err != nil {
		log.Fatalln("config:", err)
	}
	UpdateConfigEnv() // environment takes priority

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

	log.Println("ready")

	if config.Caddy != (ListenConfig{}) {
		go serveHandler("caddy", config.Caddy, ServeCaddy)
	}

	serveHandler("pages", config.Pages, ServePages)
}
