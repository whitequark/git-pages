package main

import (
	"log"
	"net/http"
	"os"
)

func main() {
	dataDir := os.Args[1]
	listenAddr := os.Args[2]

	http.HandleFunc("/", Serve(dataDir))
	err := http.ListenAndServe(listenAddr, nil)
	if err != nil {
		log.Fatalln("failed to listen:", err)
	}
}
