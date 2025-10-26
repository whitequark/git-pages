package git_pages

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
)

func ServeCaddy(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		http.Error(w, "domain parameter required", http.StatusBadRequest)
		return
	}

	// Save the backend some effort from queries that are essentially guaranteed to fail.
	// While TLS certificates may be provisionsed for IP addresses under special circumstances[^1],
	// this isn't really what git-pages is designed for, and object store accesses can cost money.
	// [^1]: https://letsencrypt.org/2025/07/01/issuing-our-first-ip-address-certificate
	if ip := net.ParseIP(domain); ip != nil {
		log.Println("caddy:", domain, 404, "(bare IP)")
		w.WriteHeader(http.StatusNotFound)
		return
	}

	found, err := backend.CheckDomain(r.Context(), strings.ToLower(domain))
	if !found {
		// If we don't serve the domain, but a fallback server does, then we should let our
		// Caddy instance request a TLS certificate. Otherwise, we'll never have an opportunity
		// to proxy the request further. (This functionality was originally added for Codeberg
		// Pages v2, which would under some circumstances return certificates with subjectAltName
		// not valid for the SNI. Go's TLS stack makes `tls.Dial` return an error for these,
		// thankfully making it unnecessary to examine X.509 certificates manually here.)
		for _, wildcardConfig := range config.Wildcard {
			if wildcardConfig.FallbackProxyTo == "" {
				continue
			}
			fallbackURL, err := url.Parse(wildcardConfig.FallbackProxyTo)
			if err != nil {
				continue
			}
			if fallbackURL.Scheme != "https" {
				continue
			}
			connectHost := fallbackURL.Host
			if fallbackURL.Port() != "" {
				connectHost += ":" + fallbackURL.Port()
			} else {
				connectHost += ":443"
			}
			log.Printf("caddy: check TLS %s", fallbackURL)
			connection, err := tls.Dial("tcp", connectHost, &tls.Config{ServerName: domain})
			if err != nil {
				continue
			}
			connection.Close()
			found = true
			break
		}
	}

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
