package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
)

func ServeCaddy(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		http.Error(w, "domain parameter required", http.StatusBadRequest)
		return
	}

	found, err := backend.CheckDomain(strings.ToLower(domain))
	if found {
		log.Println("caddy:", domain, 200)
		w.WriteHeader(http.StatusOK)
	} else if err == nil {
		log.Println("caddy:", domain, 404)
		w.WriteHeader(http.StatusNotFound)
	} else {
		log.Println("caddy:", domain, 500)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, err)
	}
}
