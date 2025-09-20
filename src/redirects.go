package main

import (
	"fmt"
	"net/http"
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

var validRedirectHTTPCodes []uint = []uint{
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

func validateRule(rule redirects.Rule) error {
	if len(rule.Params) > 0 {
		return fmt.Errorf("rules with parameters are not supported")
	}
	if rule.IsProxy() {
		return fmt.Errorf("proxy rules are not supported")
	}
	if !slices.Contains(validRedirectHTTPCodes, uint(rule.Status)) {
		return fmt.Errorf("rule cannot use status %d: must be %v",
			rule.Status, validRedirectHTTPCodes)
	}
	if strings.Contains(rule.From, "*") && !strings.HasSuffix(rule.From, "/*") {
		return fmt.Errorf("splat * must be its own final segment of the path")
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
