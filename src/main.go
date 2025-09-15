package main

import (
	"flag"
	"log"
	"net"
	"net/http"
)

var config Config

func serveHandler(name string, listen Listen, serve func(http.ResponseWriter, *http.Request)) {
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
	configPath := flag.String("config", "config.toml", "path to configuration file")
	flag.Parse()

	if err := readConfig(*configPath, &config); err != nil {
		log.Fatalln("configuration:", err)
	}

	if config.Caddy != (Listen{}) {
		go serveHandler("caddy", config.Caddy, ServeCaddy)
	}

	serveHandler("pages", config.Pages, ServePages)
}
