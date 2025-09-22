package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"

	"github.com/valyala/fasttemplate"
)

type WildcardPattern struct {
	Domain      []string
	CloneURL    *fasttemplate.Template
	IndexRepos  []*fasttemplate.Template
	FallbackURL *url.URL
}

var wildcardPatterns []*WildcardPattern

func (pattern *WildcardPattern) GetHost() string {
	parts := []string{"*"}
	parts = append(parts, pattern.Domain...)
	return strings.Join(parts, ".")
}

// Returns `subdomain, found` where if `found == true`, `subdomain` contains the part of `host`
// corresponding to the * in the domain pattern.
func (pattern *WildcardPattern) Matches(host string) (string, bool) {
	hostParts := strings.Split(host, ".")
	if len(hostParts) != 1+len(pattern.Domain) || !slices.Equal(hostParts[1:], pattern.Domain) {
		return "", false
	}
	return hostParts[0], true
}

func (pattern *WildcardPattern) IsFallbackFor(host string) bool {
	if pattern.FallbackURL == nil {
		return false
	}
	_, found := pattern.Matches(host)
	return found
}

func HandleWildcardFallback(w http.ResponseWriter, r *http.Request) (bool, error) {
	host, err := GetHost(r)
	if err != nil {
		return false, err
	}

	for _, pattern := range wildcardPatterns {
		if pattern.IsFallbackFor(host) {
			log.Printf("proxy: %s via %s", pattern.GetHost(), pattern.FallbackURL)

			(&httputil.ReverseProxy{
				Rewrite: func(r *httputil.ProxyRequest) {
					r.SetURL(pattern.FallbackURL)
					r.Out.Host = r.In.Host
					r.Out.Header["X-Forwarded-For"] = r.In.Header["X-Forwarded-For"]
				},
			}).ServeHTTP(w, r)

			return true, nil
		}
	}

	return false, nil
}

func ConfigureWildcards(config []WildcardConfig) error {
	for _, wildcardConfig := range config {
		wildcardPattern := WildcardPattern{
			Domain: strings.Split(wildcardConfig.Domain, "."),
		}

		template, err := fasttemplate.NewTemplate(wildcardConfig.CloneURL, "<", ">")
		if err != nil {
			return fmt.Errorf("wildcard pattern: clone URL: %w", err)
		} else {
			wildcardPattern.CloneURL = template
		}

		for _, indexRepo := range wildcardConfig.IndexRepos {
			template, err := fasttemplate.NewTemplate(indexRepo, "<", ">")
			if err != nil {
				return fmt.Errorf("wildcard pattern: clone URL: %w", err)
			} else {
				wildcardPattern.IndexRepos = append(wildcardPattern.IndexRepos, template)
			}
		}

		if wildcardConfig.FallbackProxyTo != "" {
			wildcardPattern.FallbackURL, err = url.Parse(wildcardConfig.FallbackProxyTo)
			if err != nil {
				return fmt.Errorf("wildcard pattern: fallback URL: %w", err)
			}
		}

		wildcardPatterns = append(wildcardPatterns, &wildcardPattern)
	}
	return nil
}
