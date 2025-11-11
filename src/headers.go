package git_pages

import (
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"

	"codeberg.org/git-pages/go-headers"
	"google.golang.org/protobuf/proto"
)

var ErrHeaderNotAllowed = errors.New("custom header not allowed")

const headersFileName string = "_headers"

// Lifted from https://docs.netlify.com/manage/routing/headers/, except for `Set-Cookie`
// the rationale for which does not apply in our environment.
var unsafeHeaders []string = []string{
	"Accept-Ranges",
	"Age",
	"Allow",
	"Alt-Svc",
	"Connection",
	"Content-Encoding",
	"Content-Length",
	"Content-Range",
	"Date",
	"Location", // use `_redirects` instead
	"Server",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func IsAllowedCustomHeader(header string) bool {
	header = textproto.CanonicalMIMEHeaderKey(header)
	switch {
	case slices.Contains(unsafeHeaders, header):
		return false // explicitly unsafe
	case slices.Contains(config.Limits.AllowedCustomHeaders, header):
		return true // explicitly allowlisted
	default:
		return false // deny by default; we don't know what the future holds
	}
}

func validateHeaderRule(rule headers.Rule) error {
	url, err := url.Parse(rule.Path)
	if err != nil {
		return fmt.Errorf("malformed path")
	}
	if url.Scheme != "" {
		return fmt.Errorf("path must not contain a scheme")
	}
	if !strings.HasPrefix(url.Path, "/") {
		return fmt.Errorf("path must start with a /")
	}
	// Per Netlify documentation:
	// > Wildcards (*) can be used at any place inside of a path segment to match any character.
	// However, we currently do not implement this, for simplicity. Instead we implement a strict
	// subset of the syntactically allowed wildcards.
	if strings.Contains(url.Path, "*") && !strings.HasSuffix(url.Path, "/*") {
		return fmt.Errorf("splat * must be its own final segment of the path")
	}
	// Note that this isn't our only line of defense against forbidden headers;
	// the purpose of this check is just to inform the uploader of a problem.
	// If the validation rules change after a manifest is uploaded, we could
	// still end up attempting to serve a forbidden header.
	for header := range rule.Headers {
		if slices.Contains(unsafeHeaders, header) {
			return fmt.Errorf("rule sets header %q (fundamentally unsafe)", header)
		}
		if !slices.Contains(config.Limits.AllowedCustomHeaders, header) {
			return fmt.Errorf("rule sets header %q (not allowlisted)", header)
		}
		if !IsAllowedCustomHeader(header) { // make sure we don't desync
			panic(errors.New("header check inconsistency"))
		}
	}
	return nil
}

// Parses redirects file and injects rules into the manifest.
func ProcessHeadersFile(manifest *Manifest) error {
	headersEntry := manifest.Contents[headersFileName]
	delete(manifest.Contents, headersFileName)
	if headersEntry == nil {
		return nil
	} else if headersEntry.GetType() != Type_InlineFile {
		return AddProblem(manifest, headersFileName,
			"not a regular file")
	}

	rules, err := headers.ParseString(string(headersEntry.GetData()))
	if err != nil {
		return AddProblem(manifest, headersFileName,
			"syntax error: %s", err)
	}

	for index, rule := range rules {
		if err := validateHeaderRule(rule); err != nil {
			AddProblem(manifest, headersFileName,
				"rule #%d %q: %s", index+1, rule.Path, err)
			continue
		}
		headerMap := []*Header{}
		for header, values := range rule.Headers {
			headerMap = append(headerMap, &Header{
				Name:   proto.String(header),
				Values: values,
			})
		}
		manifest.Headers = append(manifest.Headers, &HeaderRule{
			Path:      proto.String(rule.Path),
			HeaderMap: headerMap,
		})
	}
	return nil
}

func ApplyHeaderRules(manifest *Manifest, url *url.URL) (headers http.Header, err error) {
	headers = http.Header{}
	fromSegments := pathSegments(url.Path)
next:
	for _, rule := range manifest.Headers {
		// check if the rule matches url
		ruleURL, _ := url.Parse(*rule.Path) // pre-validated in `validateHeaderRule`
		ruleSegments := pathSegments(ruleURL.Path)
		if ruleSegments[len(ruleSegments)-1] != "*" {
			if len(ruleSegments) < len(fromSegments) {
				continue
			}
		}
		for index, ruleFromSegment := range ruleSegments {
			if ruleFromSegment == "*" {
				break
			}
			if len(fromSegments) <= index {
				continue next
			}
			if fromSegments[index] != ruleFromSegment {
				continue next
			}
		}
		// the rule has matched url, validate headers against up-to-date policy
		for _, header := range rule.HeaderMap {
			name := header.GetName()
			if !IsAllowedCustomHeader(name) {
				return nil, fmt.Errorf("%w: %s", ErrHeaderNotAllowed, name)
			}
			for _, value := range header.GetValues() {
				headers.Add(name, value)
			}
		}
		break
	}
	return
}
