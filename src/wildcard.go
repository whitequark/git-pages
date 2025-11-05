package git_pages

import (
	"crypto/tls"
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
	IndexBranch string
	FallbackURL *url.URL
	Fallback    http.Handler
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
	if pattern.Fallback == nil {
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
			pattern.Fallback.ServeHTTP(w, r)
			return true, nil
		}
	}
	return false, nil
}

func ConfigureWildcards(configs []WildcardConfig) error {
	for _, config := range configs {
		cloneURLTemplate, err := fasttemplate.NewTemplate(config.CloneURL, "<", ">")
		if err != nil {
			return fmt.Errorf("wildcard pattern: clone URL: %w", err)
		}

		var indexRepoTemplates []*fasttemplate.Template
		var indexRepoBranch string = config.IndexRepoBranch
		for _, indexRepo := range config.IndexRepos {
			indexRepoTemplate, err := fasttemplate.NewTemplate(indexRepo, "<", ">")
			if err != nil {
				return fmt.Errorf("wildcard pattern: index repo: %w", err)
			}
			indexRepoTemplates = append(indexRepoTemplates, indexRepoTemplate)
		}

		var fallbackURL *url.URL
		var fallback http.Handler
		if config.FallbackProxyTo != "" {
			fallbackURL, err = url.Parse(config.FallbackProxyTo)
			if err != nil {
				return fmt.Errorf("wildcard pattern: fallback URL: %w", err)
			}

			fallback = &httputil.ReverseProxy{
				Rewrite: func(r *httputil.ProxyRequest) {
					r.SetURL(fallbackURL)
					r.Out.Host = r.In.Host
					r.Out.Header["X-Forwarded-For"] = r.In.Header["X-Forwarded-For"]
				},
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{
						InsecureSkipVerify: config.FallbackInsecure,
					},
				},
			}
		}

		wildcardPatterns = append(wildcardPatterns, &WildcardPattern{
			Domain:      strings.Split(config.Domain, "."),
			CloneURL:    cloneURLTemplate,
			IndexRepos:  indexRepoTemplates,
			IndexBranch: indexRepoBranch,
			FallbackURL: fallbackURL,
			Fallback:    fallback,
		})
	}
	return nil
}
