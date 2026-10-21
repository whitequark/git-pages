package git_pages

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"

	"codeberg.org/git-pages/git-pages/src/names"
)

func ServeProxy(w http.ResponseWriter, r *http.Request) {
	domainParam := r.URL.Query().Get("domain")
	if domainParam == "" {
		proxyListDomains(w, r)
	} else if domain, err := names.ParseDomain(domainParam, names.ParseDomainRelaxed); err == nil {
		proxyCheckDomain(w, r, domain)
	} else {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, err)
	}
}

func proxyListDomains(w http.ResponseWriter, r *http.Request) {
	domainSet := map[string]unit{}
	for metadata, err := range backend.EnumerateManifests(r.Context()) {
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, "%s", err)
		}
		domainSet[metadata.Name.Domain.String()] = Unit
	}

	w.Header().Add("Content-Type", "application/json")
	json.NewEncoder(w).Encode(slices.Collect(maps.Keys(domainSet)))
}

func proxyCheckDomain(w http.ResponseWriter, r *http.Request, domain names.Domain) {
	var err error

	// Save the backend some effort from queries that are essentially guaranteed to fail.
	// While TLS certificates may be provisionsed for IP addresses under special circumstances[^1],
	// this isn't really what git-pages is designed for, and object store accesses can cost money.
	// [^1]: https://letsencrypt.org/2025/07/01/issuing-our-first-ip-address-certificate
	if ip := net.ParseIP(domain.String()); ip != nil {
		logc.Println(r.Context(), "proxy:", domain, 404, "(bare IP)")
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Run a cheap check as to whether we might be serving the domain.
	found := existenceCache.CheckDomain(r.Context(), domain).IsPossible()

	if found {
		// Run an expensive check as to whether we are actually serving the domain.
		found, err = backend.CheckDomain(r.Context(), domain)
	}

	if !found {
		// If we don't serve the domain, but a fallback server does, then we should let our
		// Caddy instance request a TLS certificate. Otherwise, we'll never have an opportunity
		// to proxy the request further. (This functionality was originally added for Codeberg
		// Pages v2, which would under some circumstances return certificates with subjectAltName
		// not valid for the SNI. Go's TLS stack makes `tls.Dial` return an error for these,
		// thankfully making it unnecessary to examine X.509 certificates manually here.)
		found, err = tryDialWithSNI(r.Context(), domain.String())
		if err != nil {
			logc.Printf(r.Context(), "proxy err: check SNI: %s\n", err)
		}
	}

	if found {
		logc.Println(r.Context(), "proxy:", domain, 200)
		w.WriteHeader(http.StatusOK)
	} else if err == nil {
		logc.Println(r.Context(), "proxy:", domain, 404)
		w.WriteHeader(http.StatusNotFound)
	} else {
		logc.Println(r.Context(), "proxy:", domain, 500)
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

	connectHost, connectPort := fallbackURL.Hostname(), fallbackURL.Port()
	if connectPort == "" {
		connectPort = "443"
	}

	logc.Printf(ctx, "proxy: check TLS %s", fallbackURL)
	dialer := tls.Dialer{Config: &tls.Config{ServerName: domain}}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(connectHost, connectPort))
	if err != nil {
		return false, err
	}
	connection.Close()
	return true, nil
}
