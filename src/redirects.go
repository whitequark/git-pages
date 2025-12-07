package git_pages

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/tj/go-redirects"
	"google.golang.org/protobuf/proto"
)

const RedirectsFileName string = "_redirects"

// Converts our Protobuf representation to tj/go-redirects.
func exportRedirectRule(rule *RedirectRule) *redirects.Rule {
	return &redirects.Rule{
		From:   rule.GetFrom(),
		To:     rule.GetTo(),
		Status: int(rule.GetStatus()),
		Force:  rule.GetForce(),
	}
}

func unparseRedirectRule(rule *redirects.Rule) string {
	var statusPart string
	if rule.Force {
		statusPart = fmt.Sprintf("%d!", rule.Status)
	} else {
		statusPart = fmt.Sprintf("%d", rule.Status)
	}
	parts := []string{
		rule.From,
		rule.To,
		statusPart,
	}
	for name, value := range rule.Params {
		parts = append(parts, fmt.Sprintf("%s=%s", name, value))
	}
	return strings.Join(parts, " ")
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

func validateRedirectRule(rule *redirects.Rule) error {
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
	return nil
}

// Parses redirects file and injects rules into the manifest.
func ProcessRedirectsFile(manifest *Manifest) error {
	redirectsEntry := manifest.Contents[RedirectsFileName]
	delete(manifest.Contents, RedirectsFileName)
	if redirectsEntry == nil {
		return nil
	} else if redirectsEntry.GetType() != Type_InlineFile {
		return AddProblem(manifest, RedirectsFileName,
			"not a regular file")
	}

	rules, err := redirects.ParseString(string(redirectsEntry.GetData()))
	if err != nil {
		return AddProblem(manifest, RedirectsFileName,
			"syntax error: %s", err)
	}

	for index, rule := range rules {
		if err := validateRedirectRule(&rule); err != nil {
			AddProblem(manifest, RedirectsFileName,
				"rule #%d %q: %s", index+1, unparseRedirectRule(&rule), err)
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
	var rules []string
	for _, rule := range manifest.GetRedirects() {
		rules = append(rules, unparseRedirectRule(exportRedirectRule(rule))+"\n")
	}
	return strings.Join(rules, "")
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
			AddProblem(manifest, name,
				"%s shadows redirect %q; remove the %s or use a %d! forced redirect instead",
				entryDesc,
				unparseRedirectRule(exportRedirectRule(rule)),
				entryDesc,
				rule.GetStatus(),
			)
		}
	}
}
