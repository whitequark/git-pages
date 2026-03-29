package git_pages

import (
	"context"
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
var ErrBasicAuthNotAllowed = errors.New("basic authorization not allowed")

const HeadersFileName string = "_headers"

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
		switch header {
		case "Basic-Auth":
			if !config.Limits.AllowBasicAuth {
				return fmt.Errorf("rule sets header %q (forbidden by policy)", header)
			}
		default:
			if !slices.Contains(config.Limits.AllowedCustomHeaders, header) {
				return fmt.Errorf("rule sets header %q (not allowlisted)", header)
			}
			if !IsAllowedCustomHeader(header) { // make sure we don't desync
				panic(errors.New("header check inconsistency"))
			}
		}
	}
	return nil
}

// Parses redirects file and injects rules into the manifest.
func ProcessHeadersFile(ctx context.Context, manifest *Manifest) error {
	headersEntry := manifest.Contents[HeadersFileName]
	delete(manifest.Contents, HeadersFileName)
	if headersEntry == nil {
		return nil
	}

	data, err := GetEntryContents(ctx, headersEntry)
	if errors.Is(err, ErrNotRegularFile) {
		return AddProblem(manifest, HeadersFileName,
			"not a regular file")
	} else if err != nil {
		return err
	}

	rules, err := headers.ParseString(string(data))
	if err != nil {
		return AddProblem(manifest, HeadersFileName,
			"syntax error: %s", err)
	}

	for index, rule := range rules {
		if err := validateHeaderRule(rule); err != nil {
			AddProblem(manifest, HeadersFileName,
				"rule #%d %q: %s", index+1, rule.Path, err)
			continue
		}
		headerMap := []*Header{}
		credentials := []*BasicCredential{}
		hasBasicAuth := false
		for header, values := range rule.Headers {
			switch header {
			case "Basic-Auth":
				hasBasicAuth = true
				for _, value := range values {
					for _, usernamePassword := range strings.Split(value, " ") {
						if usernamePassword == "" {
							continue
						}
						if username, password, found := strings.Cut(usernamePassword, ":"); !found {
							AddProblem(manifest, HeadersFileName,
								"rule #%d %q: malformed Basic-Auth credential", index+1, rule.Path)
							continue
						} else {
							credentials = append(credentials, &BasicCredential{
								Username: proto.String(username),
								Password: proto.String(password),
							})
						}
					}
				}
			default:
				headerMap = append(headerMap, &Header{
					Name:   proto.String(header),
					Values: values,
				})
			}
		}
		// Note that we may add an empty `headerMap` here even if only credentials are defined.
		// This is intentional: in `_headers` files processing terminates at the first matching
		// clause, and Netlify mixes Basic-Auth with all the other headers.
		manifest.Headers = append(manifest.Headers, &HeaderRule{
			Path:      proto.String(rule.Path),
			HeaderMap: headerMap,
		})
		// We're using `hasBasicAuth` instead of `len(credentials) > 0` so that if a `_headers`
		// file defines only malformed credentials, we still add a rule (that in effect always
		// denies access).
		if hasBasicAuth {
			manifest.BasicAuth = append(manifest.BasicAuth, &BasicAuthRule{
				Path:        proto.String(rule.Path),
				Credentials: credentials,
			})
		}
	}
	return nil
}

func CollectHeadersFile(manifest *Manifest) string {
	var headersRules []headers.Rule
	for _, manifestRule := range manifest.GetHeaders() {
		headersRule := headers.Rule{
			Path:    manifestRule.GetPath(),
			Headers: http.Header{},
		}
		for _, manifestHeader := range manifestRule.GetHeaderMap() {
			headersRule.Headers[manifestHeader.GetName()] = manifestHeader.GetValues()
		}
		headersRules = append(headersRules, headersRule)
	}
	return headers.Must(headers.UnparseString(headersRules))
}

func matchPathRules[
	Rule interface{ GetPath() string },
](rules []Rule, url *url.URL) (matched Rule) {
	fromSegments := pathSegments(url.Path)
next:
	for _, rule := range rules {
		// check if the rule matches url
		ruleURL, _ := url.Parse(rule.GetPath()) // pre-validated in `validateHeaderRule`
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
		matched = rule
		break
	}
	return
}

func ApplyHeaderRules(manifest *Manifest, url *url.URL) (
	headers http.Header, err error,
) {
	headers = http.Header{}
	if rule := matchPathRules(manifest.Headers, url); rule != nil {
		// the rule has matched url, validate headers against up-to-date policy
		for _, header := range rule.GetHeaderMap() {
			name := header.GetName()
			if !IsAllowedCustomHeader(name) {
				return nil, fmt.Errorf("%w: %s", ErrHeaderNotAllowed, name)
			}
			for _, value := range header.GetValues() {
				headers.Add(name, value)
			}
		}
	}
	return
}

func ApplyBasicAuthRules(manifest *Manifest, url *url.URL, r *http.Request) (bool, error) {
	if rule := matchPathRules(manifest.BasicAuth, url); rule == nil {
		// no matches, authorized by default
		return true, nil
	} else {
		// the rule has matched url, check that basic auth is allowed per up-to-date policy
		if !config.Limits.AllowBasicAuth {
			// basic auth configured in the past but not allowed any more
			return false, ErrBasicAuthNotAllowed
		}
		if username, password, ok := r.BasicAuth(); ok {
			// request has credentials, check them
			for _, credential := range rule.GetCredentials() {
				if credential.GetUsername() == username && credential.GetPassword() == password {
					// authorized!
					return true, nil
				}
			}
		}
		// request has no credentials, unauthorized
		return false, nil
	}
}
