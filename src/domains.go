package git_pages

import (
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"
)

func EffectiveTLDPlusOne(domain string) string {
	domainParts := strings.Split(domain, ".")
	for _, pattern := range wildcards {
		for _, suffix := range [][]string{pattern.PreviewDomain, pattern.Domain} {
			if len(domainParts) < len(suffix)+1 {
				continue
			}
			if slices.Equal(domainParts[len(domainParts)-len(suffix):], suffix) {
				return strings.Join(domainParts[len(domainParts)-len(suffix)-1:], ".")
			}
		}
	}

	result, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err == nil {
		return result
	}

	return domain
}
