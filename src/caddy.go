package main

import (
	"fmt"
	"log"
	"net/http"
)

func ServeCaddy(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		http.Error(w, "domain parameter required", http.StatusBadRequest)
		return
	}

	if manifest, _ := backend.GetManifest(fmt.Sprintf("%s/.index", domain)); manifest != nil {
		log.Println("caddy:", domain, 200)
		w.WriteHeader(http.StatusOK)
	} else {
		log.Println("caddy:", domain, 404)
		w.WriteHeader(http.StatusNotFound)
	}
}
