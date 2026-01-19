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
	IndexRepo     *fasttemplate.Template
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

func (pattern *WildcardPattern) ApplyTemplate(userName string, projectName string) (string, string) {
	var repoURL string
	var branch string
	repoURLTemplate := pattern.CloneURL
	if projectName == ".index" {
		repoURL = repoURLTemplate.ExecuteString(map[string]any{
			"user":    userName,
			"project": pattern.IndexRepo.ExecuteString(map[string]any{"user": userName}),
		})
		branch = pattern.IndexBranch
	} else {
		repoURL = repoURLTemplate.ExecuteString(map[string]any{
			"user":    userName,
			"project": projectName,
		})
		branch = "pages"
	}
	return repoURL, branch
}

func TranslateWildcards(configs []WildcardConfig) ([]*WildcardPattern, error) {
	var wildcardPatterns []*WildcardPattern
	for _, config := range configs {
		cloneURLTemplate, err := fasttemplate.NewTemplate(config.CloneURL, "<", ">")
		if err != nil {
			return nil, fmt.Errorf("wildcard pattern: clone URL: %w", err)
		}

		var indexRepoBranch string = config.IndexRepoBranch
		indexRepoTemplate, err := fasttemplate.NewTemplate(config.IndexRepo, "<", ">")
		if err != nil {
			return nil, fmt.Errorf("wildcard pattern: index repo: %w", err)
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
			IndexRepo:     indexRepoTemplate,
			IndexBranch:   indexRepoBranch,
			Authorization: authorization,
		})
	}
	return wildcardPatterns, nil
}
