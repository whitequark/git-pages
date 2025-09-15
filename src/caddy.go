package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
)

func ServeCaddy(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		http.Error(w, "domain parameter required", http.StatusBadRequest)
		return
	}

	wwwRoot := filepath.Join(config.DataDir, "www", domain)
	if stat, err := os.Stat(wwwRoot); err == nil && stat.IsDir() {
		log.Println("caddy:", domain, 200)
		w.WriteHeader(http.StatusOK)
	} else {
		log.Println("caddy:", domain, 404)
		w.WriteHeader(http.StatusNotFound)
	}
}
