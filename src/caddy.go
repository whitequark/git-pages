package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
)

func ServeCaddy(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("domain")
	if query == "" {
		http.Error(w, "domain parameter required", http.StatusBadRequest)
		return
	}

	// Save the backend some effort from queries that are essentially guaranteed to fail.
	// While TLS certificates may be provisionsed for IP addresses under special circumstances[^1],
	// this isn't really what git-pages is designed for, and object store accesses can cost money.
	// [^1]: https://letsencrypt.org/2025/07/01/issuing-our-first-ip-address-certificate
	if ip := net.ParseIP(query); ip != nil {
		log.Println("caddy:", query, 404, "(bare IP)")
		w.WriteHeader(http.StatusNotFound)
		return
	}

	found, err := backend.CheckDomain(strings.ToLower(query))
	if found {
		log.Println("caddy:", query, 200)
		w.WriteHeader(http.StatusOK)
	} else if err == nil {
		log.Println("caddy:", query, 404)
		w.WriteHeader(http.StatusNotFound)
	} else {
		log.Println("caddy:", query, 500)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, err)
	}
}
