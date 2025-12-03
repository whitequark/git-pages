package git_pages

import (
	"fmt"
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
}

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

func TranslateWildcards(configs []WildcardConfig) ([]*WildcardPattern, error) {
	var wildcardPatterns []*WildcardPattern
	for _, config := range configs {
		cloneURLTemplate, err := fasttemplate.NewTemplate(config.CloneURL, "<", ">")
		if err != nil {
			return nil, fmt.Errorf("wildcard pattern: clone URL: %w", err)
		}

		var indexRepoTemplates []*fasttemplate.Template
		var indexRepoBranch string = config.IndexRepoBranch
		for _, indexRepo := range config.IndexRepos {
			indexRepoTemplate, err := fasttemplate.NewTemplate(indexRepo, "<", ">")
			if err != nil {
				return nil, fmt.Errorf("wildcard pattern: index repo: %w", err)
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
				return nil, fmt.Errorf(
					"wildcard pattern: unknown authorization mechanism: %s",
					config.Authorization,
				)
			}
		}

		wildcardPatterns = append(wildcardPatterns, &WildcardPattern{
			Domain:        strings.Split(config.Domain, "."),
			CloneURL:      cloneURLTemplate,
			IndexRepos:    indexRepoTemplates,
			IndexBranch:   indexRepoBranch,
			Authorization: authorization,
		})
	}
	return wildcardPatterns, nil
}
