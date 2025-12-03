package git_pages

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
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
		logc.Println(r.Context(), "caddy:", domain, 404, "(bare IP)")
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
		found, err = tryDialWithSNI(r.Context(), domain)
		if err != nil {
			logc.Printf(r.Context(), "caddy err: check SNI: %s\n", err)
		}
	}

	if found {
		logc.Println(r.Context(), "caddy:", domain, 200)
		w.WriteHeader(http.StatusOK)
	} else if err == nil {
		logc.Println(r.Context(), "caddy:", domain, 404)
		w.WriteHeader(http.StatusNotFound)
	} else {
		logc.Println(r.Context(), "caddy:", domain, 500)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, err)
	}
}

func tryDialWithSNI(ctx context.Context, domain string) (bool, error) {
	if config.Fallback.ProxyTo == nil {
		return false, nil
	}

	fallbackURL := config.Fallback.ProxyTo
	if fallbackURL.Scheme != "https" {
		return false, nil
	}

	connectHost := fallbackURL.Host
	if fallbackURL.Port() != "" {
		connectHost += ":" + fallbackURL.Port()
	} else {
		connectHost += ":443"
	}

	logc.Printf(ctx, "caddy: check TLS %s", fallbackURL)
	connection, err := tls.Dial("tcp", connectHost, &tls.Config{ServerName: domain})
	if err != nil {
		return false, err
	}
	connection.Close()
	return true, nil
}
