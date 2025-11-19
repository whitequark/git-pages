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
	Domain        []string
	CloneURL      *fasttemplate.Template
	IndexRepos    []*fasttemplate.Template
	IndexBranch   string
	Authorization bool
	FallbackURL   *url.URL
	Fallback      http.Handler
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
	hostLen := len(hostParts)
	patternLen := len(pattern.Domain)

	// host must have at least one more part than the pattern domain
	if hostLen <= patternLen {
		return "", false
	}

	// break the host parts into <subdomain parts> and <domain parts>
	mid := hostLen - patternLen
	prefix := hostParts[:mid]
	suffix := hostParts[mid:]

	// check if the suffix matches the domain
	if !slices.Equal(suffix, pattern.Domain) {
		return "", false
	}

	// return all the subdomain parts
	subdomain := strings.Join(prefix, ".")
	return subdomain, true
}

func (pattern *WildcardPattern) ApplyTemplate(userName string, projectName string) ([]string, string) {
	var repoURLs []string
	var branch string
	repoURLTemplate := pattern.CloneURL
	if projectName == ".index" {
		for _, indexRepoTemplate := range pattern.IndexRepos {
			indexRepo := indexRepoTemplate.ExecuteString(map[string]any{"user": userName})
			repoURLs = append(repoURLs, repoURLTemplate.ExecuteString(map[string]any{
				"user":    userName,
				"project": indexRepo,
			}))
		}
		branch = pattern.IndexBranch
	} else {
		repoURLs = append(repoURLs, repoURLTemplate.ExecuteString(map[string]any{
			"user":    userName,
			"project": projectName,
		}))
		branch = "pages"
	}
	return repoURLs, branch
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

		authorization := false
		if config.Authorization != "" {
			if slices.Contains([]string{"gogs", "gitea", "forgejo"}, config.Authorization) {
				// Currently these are the only supported forges, and the authorization mechanism
				// is the same for all of them.
				authorization = true
			} else {
				return fmt.Errorf(
					"wildcard pattern: unknown authorization mechanism: %s",
					config.Authorization,
				)
			}
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
			Domain:        strings.Split(config.Domain, "."),
			CloneURL:      cloneURLTemplate,
			IndexRepos:    indexRepoTemplates,
			IndexBranch:   indexRepoBranch,
			Authorization: authorization,
			FallbackURL:   fallbackURL,
			Fallback:      fallback,
		})
	}
	return nil
}
