package git_pages

import (
	"fmt"
	"slices"
	"strings"

	"github.com/valyala/fasttemplate"
)

type WildcardPattern struct {
	Domain             []string
	PreviewDomain      []string
	CloneURL           *fasttemplate.Template
	IndexRepo          *fasttemplate.Template
	IndexBranch        string
	Authorization      bool
	MaxPreviewLifetime uint
}

func (pattern *WildcardPattern) GetHost() string {
	parts := []string{"*"}
	parts = append(parts, pattern.Domain...)
	return strings.Join(parts, ".")
}

type WildcardDomainKind int

const (
	WildcardDomainPrimary WildcardDomainKind = iota
	WildcardDomainPreview
	WildcardDomainAny
)

// Returns `subdomain, found` where if `found == true`, `subdomain` contains the part of `host`
// corresponding to the * in the domain pattern.
func (pattern *WildcardPattern) Matches(host string, kind WildcardDomainKind) (string, bool) {
	var suffixParts []string
	switch kind {
	case WildcardDomainPrimary:
		suffixParts = pattern.Domain
	case WildcardDomainPreview:
		suffixParts = pattern.PreviewDomain
	case WildcardDomainAny:
		if userName, found := pattern.Matches(host, WildcardDomainPrimary); found {
			return userName, found
		} else if userName, found := pattern.Matches(host, WildcardDomainPreview); found {
			return userName, found
		}
		return "", false
	default:
		panic("invalid wildcard domain kind")
	}

	hostParts := strings.Split(host, ".")
	if len(suffixParts) == 0 {
		return "", false
	} else if len(hostParts) != len(suffixParts)+1 {
		return "", false
	} else if !slices.Equal(hostParts[1:], suffixParts) {
		return "", false
	} else {
		return hostParts[0], true
	}
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

func TranslateWildcards(wildcardConfigs []WildcardConfig) ([]*WildcardPattern, error) {
	var wildcardPatterns []*WildcardPattern
	for _, wildcardConfig := range wildcardConfigs {
		cloneURLTemplate, err := fasttemplate.NewTemplate(wildcardConfig.CloneURL, "<", ">")
		if err != nil {
			return nil, fmt.Errorf("wildcard pattern: clone URL: %w", err)
		}

		var indexRepoBranch string = wildcardConfig.IndexRepoBranch
		indexRepoTemplate, err := fasttemplate.NewTemplate(wildcardConfig.IndexRepo, "<", ">")
		if err != nil {
			return nil, fmt.Errorf("wildcard pattern: index repo: %w", err)
		}

		authorization := false
		if wildcardConfig.Authorization != "" {
			if slices.Contains([]string{"gogs", "gitea", "forgejo"}, wildcardConfig.Authorization) {
				// Currently these are the only supported forges, and the authorization mechanism
				// is the same for all of them.
				authorization = true
			} else {
				return nil, fmt.Errorf(
					"wildcard pattern: unknown authorization mechanism: %s",
					wildcardConfig.Authorization,
				)
			}
		}

		if !config.Feature("preview") {
			wildcardConfig.PreviewDomain = ""
		}
		if wildcardConfig.PreviewDomain != "" {
			if wildcardConfig.Authorization != "forgejo" {
				return nil, fmt.Errorf(
					"wildcard pattern: previews require Forgejo authorization",
				)
			}
		}

		wildcardPatterns = append(wildcardPatterns, &WildcardPattern{
			Domain:             strings.Split(wildcardConfig.Domain, "."),
			PreviewDomain:      strings.Split(wildcardConfig.PreviewDomain, "."),
			CloneURL:           cloneURLTemplate,
			IndexRepo:          indexRepoTemplate,
			IndexBranch:        indexRepoBranch,
			Authorization:      authorization,
			MaxPreviewLifetime: wildcardConfig.MaxPreviewLifetime,
		})
	}
	return wildcardPatterns, nil
}
