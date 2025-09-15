package main

import (
	"flag"
	"log"
	"net"
	"net/http"
)

var config Config

func main() {
	configPath := flag.String("config", "config.toml", "path to configuration file")
	flag.Parse()

	if err := readConfig(*configPath, &config); err != nil {
		log.Fatalln("failed to read configuration:", err)
	}

	listener, err := net.Listen(config.Listen.Protocol, config.Listen.Address)
	if err != nil {
		log.Fatalln("failed to listen:", err)
	}

	http.HandleFunc("/", Serve)
	if err := http.Serve(listener, nil); err != nil {
		log.Fatalln("failed to serve:", err)
	}
}
