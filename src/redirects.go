package git_pages

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"codeberg.org/git-pages/git-pages/src/names"
	"codeberg.org/git-pages/go-redirects"
	"google.golang.org/protobuf/proto"
)

const RedirectsFileName string = "_redirects"

// Converts our Protobuf representation to tj/go-redirects.
func exportRedirectRule(rule *RedirectRule) redirects.Rule {
	return redirects.Rule{
		From:   rule.GetFrom(),
		To:     rule.GetTo(),
		Status: int(rule.GetStatus()),
		Force:  rule.GetForce(),
	}
}

var validRedirectHTTPStatuses []int = []int{
	http.StatusOK,
	http.StatusMovedPermanently,
	http.StatusFound,
	http.StatusSeeOther,
	http.StatusTemporaryRedirect,
	http.StatusPermanentRedirect,
	http.StatusForbidden,
	http.StatusNotFound,
	http.StatusGone,
	http.StatusTeapot,
	http.StatusUnavailableForLegalReasons,
}

func Is3xxHTTPStatus(status int) bool {
	return status >= 300 && status <= 399
}

func validateRedirectRule(project names.Project, rule *redirects.Rule) error {
	if len(rule.Params) > 0 {
		return fmt.Errorf("rules with parameters are not supported")
	}
	if !slices.Contains(validRedirectHTTPStatuses, rule.Status) {
		return fmt.Errorf("rule cannot use status %d: must be %v",
			rule.Status, validRedirectHTTPStatuses)
	}
	fromURL, err := url.Parse(rule.From)
	if err != nil {
		return fmt.Errorf("malformed 'from' URL")
	}
	if fromURL.Scheme != "" {
		return fmt.Errorf("'from' URL path must not contain a scheme")
	}
	if !strings.HasPrefix(fromURL.Path, "/") {
		return fmt.Errorf("'from' URL path must start with a /")
	}
	if strings.Contains(fromURL.Path, "*") && !strings.HasSuffix(fromURL.Path, "/*") {
		return fmt.Errorf("splat * must be its own final segment of the path")
	}
	toURL, err := url.Parse(rule.To)
	if err != nil {
		return fmt.Errorf("malformed 'to' URL")
	}
	if !Is3xxHTTPStatus(rule.Status) {
		if !strings.HasPrefix(toURL.Path, "/") {
			return fmt.Errorf("'to' URL path must start with a / for non-3xx status rules")
		}
		if toURL.Host != "" {
			return fmt.Errorf("'to' URL may only include a hostname for 3xx status rules")
		}
	}
	// After v0.9.1, we've changed the redirect handling so that the 'to' URL for a rewrite rule
	// must be a valid URL pointing to this site, rather than a valid path within the manifest
	// (which was the old, incorrect interpretation). This broke every SPA on git-pages that was
	// deployed to a non-index project. Unfortunately, we can't do much more than diagnosing
	// the problematic pattern at deploy time.
	if rule.Status == http.StatusOK && !project.IsIndex() && toURL.Host == "" &&
		!strings.HasPrefix(toURL.Path, fmt.Sprintf("/%s/", project)) {
		return fmt.Errorf("'to' URL must start with '/%s/' for a 200 status rule", project)
	}
	return nil
}

// Parses redirects file and injects rules into the manifest.
func ProcessRedirectsFile(ctx context.Context, project names.Project, manifest *Manifest) error {
	redirectsEntry := manifest.Contents[RedirectsFileName]
	delete(manifest.Contents, RedirectsFileName)
	if redirectsEntry == nil {
		return nil
	}
	manifest.Redirects = []*RedirectRule{}

	data, err := GetEntryContents(ctx, redirectsEntry)
	if errors.Is(err, ErrNotRegularFile) {
		return AddProblem(manifest, RedirectsFileName,
			"not a regular file")
	} else if err != nil {
		return err
	}

	rules, err := redirects.ParseString(string(data))
	if err != nil {
		return AddProblem(manifest, RedirectsFileName,
			"syntax error: %s", err)
	}

	for index, rule := range rules {
		if err := validateRedirectRule(project, &rule); err != nil {
			ruleString := redirects.Must(redirects.UnparseString([]redirects.Rule{rule}))
			AddProblem(manifest, RedirectsFileName,
				"rule #%d %q: %s", index+1, strings.TrimSpace(ruleString), err)
			continue
		}
		manifest.Redirects = append(manifest.Redirects, &RedirectRule{
			From:   proto.String(rule.From),
			To:     proto.String(rule.To),
			Status: proto.Uint32(uint32(rule.Status)),
			Force:  proto.Bool(rule.Force),
		})
	}
	return nil
}

func CollectRedirectsFile(manifest *Manifest) string {
	var rules []redirects.Rule
	for _, rule := range manifest.GetRedirects() {
		rules = append(rules, exportRedirectRule(rule))
	}
	return redirects.Must(redirects.UnparseString(rules))
}

func pathSegments(path string) []string {
	return strings.Split(strings.TrimPrefix(path, "/"), "/")
}

func toOrFromComponent(to, from string) string {
	if to == "" {
		return from
	} else {
		return to
	}
}

type RedirectKind int

const (
	RedirectAny RedirectKind = iota
	RedirectNormal
	RedirectForce
)

func ApplyRedirectRules(
	manifest *Manifest, fromURL *url.URL, kind RedirectKind,
) (
	rule *RedirectRule, toURL *url.URL, status int,
) {
	fromSegments := pathSegments(fromURL.Path)
next:
	for _, rule = range manifest.Redirects {
		switch {
		case kind == RedirectNormal && *rule.Force:
			continue
		case kind == RedirectForce && !*rule.Force:
			continue
		}
		// check if the rule matches fromURL
		ruleFromURL, _ := url.Parse(*rule.From) // pre-validated in `validateRedirectRule`
		if ruleFromURL.Scheme != "" && fromURL.Scheme != ruleFromURL.Scheme {
			continue
		}
		if ruleFromURL.Host != "" && fromURL.Hostname() != ruleFromURL.Host {
			continue
		}
		ruleFromSegments := pathSegments(ruleFromURL.Path)
		splatSegments := []string{}
		if ruleFromSegments[len(ruleFromSegments)-1] != "*" {
			if len(ruleFromSegments) < len(fromSegments) {
				continue
			}
		}
		for index, ruleFromSegment := range ruleFromSegments {
			if ruleFromSegment == "*" {
				splatSegments = fromSegments[index:]
				break
			}
			if len(fromSegments) <= index {
				continue next
			}
			if fromSegments[index] != ruleFromSegment {
				continue next
			}
		}
		// the rule has matched fromURL, figure out where to redirect
		ruleToURL, _ := url.Parse(*rule.To) // pre-validated in `validateRule`
		toSegments := []string{}
		for _, ruleToSegment := range pathSegments(ruleToURL.Path) {
			if ruleToSegment == ":splat" {
				toSegments = append(toSegments, splatSegments...)
			} else {
				toSegments = append(toSegments, ruleToSegment)
			}
		}
		toURL = &url.URL{
			Scheme:   toOrFromComponent(ruleToURL.Scheme, fromURL.Scheme),
			Host:     toOrFromComponent(ruleToURL.Host, fromURL.Host),
			Path:     "/" + strings.Join(toSegments, "/"),
			RawQuery: fromURL.RawQuery,
			Fragment: toOrFromComponent(ruleToURL.Fragment, fromURL.Fragment),
		}
		status = int(*rule.Status)
		return
	}
	// no redirect found
	rule = nil
	return
}

func redirectHasSplat(rule *RedirectRule) bool {
	ruleFromURL, _ := url.Parse(*rule.From) // pre-validated in `validateRedirectRule`
	ruleFromSegments := pathSegments(ruleFromURL.Path)
	return slices.Contains(ruleFromSegments, "*")
}

func LintRedirects(manifest *Manifest) {
	for name, entry := range manifest.GetContents() {
		nameURL, err := url.Parse("/" + name)
		if err != nil {
			continue
		}

		// Check if the entry URL would trigger a non-forced redirect if the entry didn't exist.
		// If the redirect matches exactly one URL (i.e. has no splat) then it will never be
		// triggered and an issue is reported; if the rule has a splat, it will always be possible
		// to trigger it, as it matches an infinite number of URLs.
		rule, _, _ := ApplyRedirectRules(manifest, nameURL, RedirectNormal)
		if rule != nil && !redirectHasSplat(rule) {
			entryDesc := "file"
			if entry.GetType() == Type_Directory {
				entryDesc = "directory"
			}
			ruleString := redirects.Must(redirects.UnparseString(
				[]redirects.Rule{exportRedirectRule(rule)}))
			AddProblem(manifest, name,
				"%s shadows redirect %q; remove the %s or use a %d! forced redirect instead",
				entryDesc,
				strings.TrimSpace(ruleString),
				entryDesc,
				rule.GetStatus(),
			)
		}
	}
}
