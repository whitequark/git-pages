package main

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/tj/go-redirects"
	"google.golang.org/protobuf/proto"
)

const redirectsFile string = "_redirects"

func unparseRule(rule redirects.Rule) string {
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

var validRedirectHTTPStatuses []uint = []uint{
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

func Is3xxHTTPStatus(status uint) bool {
	return status >= 300 && status <= 399
}

func validateRule(rule redirects.Rule) error {
	if len(rule.Params) > 0 {
		return fmt.Errorf("rules with parameters are not supported")
	}
	if !slices.Contains(validRedirectHTTPStatuses, uint(rule.Status)) {
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
	if !strings.HasPrefix(toURL.Path, "/") {
		return fmt.Errorf("'to' URL path must start with a /")
	}
	if toURL.Host != "" && !Is3xxHTTPStatus(uint(rule.Status)) {
		return fmt.Errorf("'to' URL may only include a hostname for 3xx status rules")
	}
	if rule.Force {
		return fmt.Errorf("force redirects are not supported")
	}
	return nil
}

// Parses redirects file and injects rules into the manifest.
func ProcessRedirects(manifest *Manifest) error {
	redirectsEntry := manifest.Contents[redirectsFile]
	delete(manifest.Contents, redirectsFile)
	if redirectsEntry == nil {
		return nil
	} else if redirectsEntry.GetType() != Type_InlineFile {
		return AddProblem(manifest, redirectsFile,
			"not a regular file")
	}

	rules, err := redirects.ParseString(string(redirectsEntry.GetData()))
	if err != nil {
		return AddProblem(manifest, redirectsFile,
			"syntax error: %s", err)
	}

	for index, rule := range rules {
		if err := validateRule(rule); err != nil {
			return AddProblem(manifest, redirectsFile,
				"rule #%d %q: %s", index+1, unparseRule(rule), err)
		}
		manifest.Redirects = append(manifest.Redirects, &Redirect{
			From:   proto.String(rule.From),
			To:     proto.String(rule.To),
			Status: proto.Uint32(uint32(rule.Status)),
			Force:  proto.Bool(rule.Force),
		})
	}
	return nil
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

func ApplyRedirects(manifest *Manifest, fromURL *url.URL) (toURL *url.URL, status uint) {
	fromSegments := pathSegments(fromURL.Path)
next:
	for _, rule := range manifest.Redirects {
		// check if the rule matches fromURL
		ruleFromURL, _ := url.Parse(*rule.From) // pre-validated in `validateRule`
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
		status = uint(*rule.Status)
		break
	}
	// no redirect found
	return
}
