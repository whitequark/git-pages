package main

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
)

type AuthError struct {
	code  int
	error string
}

func (e AuthError) Error() string {
	return e.error
}

func IsUnauthorized(err error) bool {
	var authErr AuthError
	if errors.As(err, &authErr) {
		return authErr.code == http.StatusUnauthorized
	}
	return false
}

func InsecureMode() bool {
	return os.Getenv("INSECURE") == "very"
}

func GetHost(r *http.Request) string {
	// FIXME: handle IDNA
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		// dirty but the go stdlib doesn't have a "split port if present" function
		host = r.Host
	}
	return host
}

func GetProjectName(r *http.Request) (string, error) {
	// path must be either `/` or `/foo/` (`/foo` is accepted as an alias)
	path := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/")
	if strings.HasPrefix(path, ".") {
		return "", AuthError{http.StatusBadRequest, "directory name %s is reserved"}
	} else if strings.Contains(path, "/") {
		return "", AuthError{http.StatusBadRequest, "directories nested too deep"}
	}

	if path == "" {
		// path `/` corresponds to pseudo-project `.index`
		return ".index", nil
	} else {
		return path, nil
	}
}

type Authorization struct {
	// If `nil`, any URL is allowed. If not, only those in the set are allowed.
	repoURLs []string
}

func authorizeDNSChallenge(r *http.Request) (*Authorization, error) {
	host := GetHost(r)

	authorization := r.Header.Get("Authorization")
	if authorization == "" {
		return nil, AuthError{http.StatusUnauthorized, "missing Authorization header"}
	}

	scheme, param, success := strings.Cut(authorization, " ")
	if !success {
		return nil, AuthError{http.StatusBadRequest, "malformed Authorization header"}
	}

	if scheme != "Pages" && scheme != "Basic" {
		return nil, AuthError{http.StatusBadRequest, "unknown Authorization scheme"}
	}

	// services like GitHub and Gogs cannot send a custom Authorization: header, but supplying
	// username and password in the URL is basically just as good
	if scheme == "Basic" {
		basicParam, err := base64.StdEncoding.DecodeString(param)
		if err != nil {
			return nil, AuthError{http.StatusBadRequest, "malformed Authorization: Basic header"}
		}

		username, password, found := strings.Cut(string(basicParam), ":")
		if !found {
			return nil, AuthError{http.StatusBadRequest, "malformed Authorization: Basic parameter"}
		}

		if username != "Pages" {
			return nil, AuthError{http.StatusUnauthorized, "unexpected Authorization: Basic username"}
		}

		param = password
	}

	challengeHostname := fmt.Sprintf("_git-pages-challenge.%s", host)
	actualChallenges, err := net.LookupTXT(challengeHostname)
	if err != nil {
		return nil, AuthError{http.StatusUnauthorized,
			fmt.Sprintf("failed to look up DNS challenge: %s TXT", challengeHostname)}
	}

	expectedChallenge := fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "%s %s", host, param)))
	if !slices.Contains(actualChallenges, expectedChallenge) {
		return nil, AuthError{http.StatusUnauthorized, fmt.Sprintf(
			"defeated by DNS challenge: %s TXT %v does not include %s",
			challengeHostname,
			actualChallenges,
			expectedChallenge,
		)}
	}

	return &Authorization{}, nil
}

func authorizeDNSAllowlist(r *http.Request) (*Authorization, error) {
	host := GetHost(r)

	allowlistHostname := fmt.Sprintf("_git-pages-repository.%s", host)
	repoURLs, err := net.LookupTXT(allowlistHostname)
	if err != nil {
		return nil, AuthError{http.StatusUnauthorized,
			fmt.Sprintf("failed to look up DNS repository allowlist: %s TXT", allowlistHostname)}
	}

	for _, repoURL := range repoURLs {
		if parsedURL, err := url.Parse(repoURL); err != nil {
			return nil, AuthError{http.StatusBadRequest,
				fmt.Sprintf("failed to parse URL: %s TXT %q", allowlistHostname, repoURL)}
		} else if !parsedURL.IsAbs() {
			return nil, AuthError{http.StatusBadRequest,
				fmt.Sprintf("repository URL is not absolute: %s TXT %q", allowlistHostname, repoURL)}
		}
	}

	return &Authorization{repoURLs}, err
}

func authorizeWildcardMatch(r *http.Request) (*Authorization, error) {
	host := GetHost(r)
	hostParts := strings.Split(host, ".")

	projectName, err := GetProjectName(r)
	if err != nil {
		return nil, err
	}

	if slices.Equal(hostParts[1:], wildcardPattern.Domain) {
		userName := hostParts[0]
		var repoURLs []string
		repoURLTemplate := wildcardPattern.CloneURL
		if projectName == ".index" {
			for _, indexRepoTemplate := range wildcardPattern.IndexRepos {
				indexRepo := indexRepoTemplate.ExecuteString(map[string]any{"user": userName})
				repoURLs = append(repoURLs, repoURLTemplate.ExecuteString(map[string]interface{}{
					"user":    userName,
					"project": indexRepo,
				}))
			}
		} else {
			repoURLs = append(repoURLs, repoURLTemplate.ExecuteString(map[string]interface{}{
				"user":    userName,
				"project": projectName,
			}))
		}
		return &Authorization{repoURLs}, nil
	} else {
		return nil, AuthError{
			http.StatusUnauthorized,
			fmt.Sprintf("domain %s does not match wildcard *.%s", host, config.Wildcard.Domain),
		}
	}
}

// Returns `repoURLs, err` where if `err == nil` then the request is authorized to clone from
// any repository URL included in `repoURLs` (by case-insensitive comparison), or any URL at all
// if `repoURLs == nil`.
func AuthorizeRequest(r *http.Request) (*Authorization, error) {
	causes := []error{AuthError{http.StatusUnauthorized, "unauthorized"}}

	if InsecureMode() {
		log.Println("auth: INSECURE mode: allow *")
		return &Authorization{}, nil // for testing only
	}

	// DNS challenge gives absolute authority.
	auth, err := authorizeDNSChallenge(r)
	if err != nil && IsUnauthorized(err) {
		causes = append(causes, err)
	} else if err != nil { // bad request
		return nil, err
	} else {
		log.Println("auth: DNS challenge: allow *")
		return auth, nil
	}

	// DNS allowlist gives authority to update but not delete.
	if r.Method == http.MethodPut || r.Method == http.MethodPut {
		auth, err = authorizeDNSAllowlist(r)
		if err != nil && IsUnauthorized(err) {
			causes = append(causes, err)
		} else if err != nil { // bad request
			return nil, err
		} else {
			log.Printf("auth: DNS allowlist: allow %v\n", auth.repoURLs)
			return auth, nil
		}
	}

	// Wildcard match is only available for webhooks, not the REST API.
	if r.Method == http.MethodPost {
		auth, err = authorizeWildcardMatch(r)
		if err != nil && IsUnauthorized(err) {
			causes = append(causes, err)
		} else if err != nil { // bad request
			return nil, err
		} else {
			log.Printf("auth: wildcard *.%s: allow %v\n",
				config.Wildcard.Domain, auth.repoURLs)
			return auth, nil
		}
	}

	return nil, errors.Join(causes...)
}

func AuthorizeRepository(repoURL string, auth *Authorization) error {
	if auth.repoURLs == nil {
		return nil // any
	}

	allowed := false
	for _, allowedRepoURL := range auth.repoURLs {
		if strings.EqualFold(repoURL, allowedRepoURL) {
			allowed = true
			break
		}
	}

	if allowed {
		return nil
	} else {
		return AuthError{
			http.StatusUnauthorized,
			fmt.Sprintf("clone URL not in allowlist %v", auth.repoURLs),
		}
	}
}

// The purpose of `allowRepoURLs` is to make sure that only authorized content is deployed
// to the site despite the fact that the non-shared-secret authorization methods allow anyone
// to impersonate the legitimate webhook sender. (If switching to another repository URL would
// be catastrophic, then so would be switching to a different branch.)
func AuthorizeBranch(branch string, auth *Authorization) error {
	if auth.repoURLs == nil {
		return nil // any
	}

	if branch == "pages" {
		return nil
	} else {
		return AuthError{
			http.StatusUnauthorized,
			fmt.Sprintf("branch %s: password authorization required", branch),
		}
	}
}
