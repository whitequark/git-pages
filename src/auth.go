package git_pages

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
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

func authorizeInsecure() *Authorization {
	if config.Insecure { // for testing only
		log.Println("auth: INSECURE mode")
		return &Authorization{
			repoURLs: nil,
			branch:   "pages",
		}
	}
	return nil
}

func GetHost(r *http.Request) (string, error) {
	// FIXME: handle IDNA
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		// dirty but the go stdlib doesn't have a "split port if present" function
		host = r.Host
	}
	if strings.HasPrefix(host, ".") {
		return "", AuthError{http.StatusBadRequest,
			fmt.Sprintf("host name %q is reserved", host)}
	}
	return host, nil
}

func GetProjectName(r *http.Request) (string, error) {
	// path must be either `/` or `/foo/` (`/foo` is accepted as an alias)
	path := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/"), "/")
	if path == ".index" || strings.HasPrefix(path, ".index/") {
		return "", AuthError{http.StatusBadRequest,
			fmt.Sprintf("directory name %q is reserved", ".index")}
	} else if strings.Contains(path, "/") {
		return "", AuthError{http.StatusBadRequest,
			"directories nested too deep"}
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
	// Only the exact branch is allowed.
	branch string
}

func authorizeDNSChallenge(r *http.Request) (*Authorization, error) {
	host, err := GetHost(r)
	if err != nil {
		return nil, err
	}

	authorization := r.Header.Get("Authorization")
	if authorization == "" {
		return nil, AuthError{http.StatusUnauthorized,
			"missing Authorization header"}
	}

	scheme, param, success := strings.Cut(authorization, " ")
	if !success {
		return nil, AuthError{http.StatusBadRequest,
			"malformed Authorization header"}
	}

	if scheme != "Pages" && scheme != "Basic" {
		return nil, AuthError{http.StatusBadRequest,
			"unknown Authorization scheme"}
	}

	// services like GitHub and Gogs cannot send a custom Authorization: header, but supplying
	// username and password in the URL is basically just as good
	if scheme == "Basic" {
		basicParam, err := base64.StdEncoding.DecodeString(param)
		if err != nil {
			return nil, AuthError{http.StatusBadRequest,
				"malformed Authorization: Basic header"}
		}

		username, password, found := strings.Cut(string(basicParam), ":")
		if !found {
			return nil, AuthError{http.StatusBadRequest,
				"malformed Authorization: Basic parameter"}
		}

		if username != "Pages" {
			return nil, AuthError{http.StatusUnauthorized,
				"unexpected Authorization: Basic username"}
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

	return &Authorization{
		repoURLs: nil, // any
		branch:   "pages",
	}, nil
}

func authorizeDNSAllowlist(r *http.Request) (*Authorization, error) {
	host, err := GetHost(r)
	if err != nil {
		return nil, err
	}

	allowlistHostname := fmt.Sprintf("_git-pages-repository.%s", host)
	records, err := net.LookupTXT(allowlistHostname)
	if err != nil {
		return nil, AuthError{http.StatusUnauthorized,
			fmt.Sprintf("failed to look up DNS repository allowlist: %s TXT", allowlistHostname)}
	}

	var (
		repoURLs []string
		errs     []error
	)
	for _, record := range records {
		if parsedURL, err := url.Parse(record); err != nil {
			errs = append(errs, fmt.Errorf("failed to parse URL: %s TXT %q", allowlistHostname, record))
		} else if !parsedURL.IsAbs() {
			errs = append(errs, fmt.Errorf("repository URL is not absolute: %s TXT %q", allowlistHostname, record))
		} else {
			repoURLs = append(repoURLs, record)
		}
	}

	if len(repoURLs) == 0 {
		if len(records) > 0 {
			errs = append([]error{AuthError{http.StatusUnauthorized,
				fmt.Sprintf("no valid DNS TXT records for %s", allowlistHostname)}},
				errs...)
			return nil, joinErrors(errs...)
		} else {
			return nil, AuthError{http.StatusUnauthorized,
				fmt.Sprintf("no DNS TXT records found for %s", allowlistHostname)}
		}
	}

	return &Authorization{
		repoURLs: repoURLs,
		branch:   "pages",
	}, err
}

// used for `/.git-pages/...` metadata
func authorizeWildcardMatchHost(r *http.Request, pattern *WildcardPattern) (*Authorization, error) {
	host, err := GetHost(r)
	if err != nil {
		return nil, err
	}

	if _, found := pattern.Matches(host); found {
		return &Authorization{
			repoURLs: []string{},
			branch:   "",
		}, nil
	} else {
		return nil, AuthError{
			http.StatusUnauthorized,
			fmt.Sprintf("domain %s does not match wildcard %s", host, pattern.GetHost()),
		}
	}
}

// used for updates to site content
func authorizeWildcardMatchSite(r *http.Request, pattern *WildcardPattern) (*Authorization, error) {
	host, err := GetHost(r)
	if err != nil {
		return nil, err
	}

	projectName, err := GetProjectName(r)
	if err != nil {
		return nil, err
	}

	if userName, found := pattern.Matches(host); found {
		var repoURLs []string
		var branch string
		repoURLTemplate := pattern.CloneURL
		if projectName == ".index" {
			for _, indexRepoTemplate := range pattern.IndexRepos {
				indexRepo := indexRepoTemplate.ExecuteString(map[string]any{"user": userName})
				repoURLs = append(repoURLs, repoURLTemplate.ExecuteString(map[string]any{
					"user":    userName,
					"project": indexRepo,
				}))
			}
			branch = pattern.IndexBranch
		} else {
			repoURLs = append(repoURLs, repoURLTemplate.ExecuteString(map[string]any{
				"user":    userName,
				"project": projectName,
			}))
			branch = "pages"
		}
		return &Authorization{repoURLs, branch}, nil
	} else {
		return nil, AuthError{
			http.StatusUnauthorized,
			fmt.Sprintf("domain %s does not match wildcard %s", host, pattern.GetHost()),
		}
	}
}

// used for compatibility with Codeberg Pages v2
// see https://docs.codeberg.org/codeberg-pages/using-custom-domain/
func authorizeCodebergPagesV2(r *http.Request) (*Authorization, error) {
	host, err := GetHost(r)
	if err != nil {
		return nil, err
	}

	dnsRecords := []string{}

	cnameRecord, err := net.LookupCNAME(host)
	// "LookupCNAME does not return an error if host does not contain DNS "CNAME" records,
	// as long as host resolves to address records.
	if err == nil && cnameRecord != host {
		// LookupCNAME() returns a domain with the root label, i.e. `username.codeberg.page.`,
		// with the trailing dot
		dnsRecords = append(dnsRecords, strings.TrimSuffix(cnameRecord, "."))
	}

	txtRecords, err := net.LookupTXT(host)
	if err == nil {
		dnsRecords = append(dnsRecords, txtRecords...)
	}

	if len(dnsRecords) > 0 {
		log.Printf("auth: %s TXT/CNAME: %q\n", host, dnsRecords)
	}

	for _, dnsRecord := range dnsRecords {
		domainParts := strings.Split(dnsRecord, ".")
		slices.Reverse(domainParts)
		if domainParts[0] == "" {
			domainParts = domainParts[1:]
		}
		if len(domainParts) >= 3 && len(domainParts) <= 5 {
			if domainParts[0] == "page" && domainParts[1] == "codeberg" {
				// map of domain names to allowed repository and branch:
				//  * {username}.codeberg.page =>
				//      https://codeberg.org/{username}/pages.git#main
				//  * {reponame}.{username}.codeberg.page =>
				//      https://codeberg.org/{username}/{reponame}.git#pages
				//  * {branch}.{reponame}.{username}.codeberg.page =>
				//      https://codeberg.org/{username}/{reponame}.git#{branch}
				username := domainParts[2]
				reponame := "pages"
				branch := "main"
				if len(domainParts) >= 4 {
					reponame = domainParts[3]
					branch = "pages"
				}
				if len(domainParts) == 5 {
					branch = domainParts[4]
				}
				return &Authorization{
					repoURLs: []string{
						fmt.Sprintf("https://codeberg.org/%s/%s.git", username, reponame),
					},
					branch: branch,
				}, nil
			}
		}
	}

	return nil, AuthError{
		http.StatusUnauthorized,
		fmt.Sprintf("domain %s does not have Codeberg Pages TXT or CNAME records", host),
	}
}

func AuthorizeMetadataRetrieval(r *http.Request) (*Authorization, error) {
	causes := []error{AuthError{http.StatusUnauthorized, "unauthorized"}}

	auth := authorizeInsecure()
	if auth != nil {
		return auth, nil
	}

	auth, err := authorizeDNSChallenge(r)
	if err != nil && IsUnauthorized(err) {
		causes = append(causes, err)
	} else if err != nil { // bad request
		return nil, err
	} else {
		log.Println("auth: DNS challenge")
		return auth, nil
	}

	for _, pattern := range wildcardPatterns {
		auth, err = authorizeWildcardMatchHost(r, pattern)
		if err != nil && IsUnauthorized(err) {
			causes = append(causes, err)
		} else if err != nil { // bad request
			return nil, err
		} else {
			log.Printf("auth: wildcard %s\n", pattern.GetHost())
			return auth, nil
		}
	}

	if config.Feature("codeberg-pages-compat") {
		auth, err = authorizeCodebergPagesV2(r)
		if err != nil && IsUnauthorized(err) {
			causes = append(causes, err)
		} else if err != nil { // bad request
			return nil, err
		} else {
			log.Printf("auth: codeberg %s\n", r.Host)
			return auth, nil
		}
	}

	return nil, joinErrors(causes...)
}

// Returns `repoURLs, err` where if `err == nil` then the request is authorized to clone from
// any repository URL included in `repoURLs` (by case-insensitive comparison), or any URL at all
// if `repoURLs == nil`.
func AuthorizeUpdateFromRepository(r *http.Request) (*Authorization, error) {
	causes := []error{AuthError{http.StatusUnauthorized, "unauthorized"}}

	if err := CheckForbiddenDomain(r); err != nil {
		return nil, err
	}

	auth := authorizeInsecure()
	if auth != nil {
		return auth, nil
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
	if r.Method == http.MethodPut || r.Method == http.MethodPost {
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
		for _, pattern := range wildcardPatterns {
			auth, err = authorizeWildcardMatchSite(r, pattern)
			if err != nil && IsUnauthorized(err) {
				causes = append(causes, err)
			} else if err != nil { // bad request
				return nil, err
			} else {
				log.Printf("auth: wildcard %s: allow %v\n", pattern.GetHost(), auth.repoURLs)
				return auth, nil
			}
		}

		if config.Feature("codeberg-pages-compat") {
			auth, err = authorizeCodebergPagesV2(r)
			if err != nil && IsUnauthorized(err) {
				causes = append(causes, err)
			} else if err != nil { // bad request
				return nil, err
			} else {
				log.Printf("auth: codeberg %s: allow %v branch %s\n",
					r.Host, auth.repoURLs, auth.branch)
				return auth, nil
			}
		}
	}

	return nil, joinErrors(causes...)
}

var repoURLSchemeAllowlist []string = []string{"ssh", "http", "https"}

func AuthorizeRepository(rawRepoURL string, auth *Authorization) error {
	// Regardless of any other authorization, only the allowlisted URL schemes
	// may ever be cloned from, so this check has to come first.
	repoURL, err := url.Parse(rawRepoURL)
	if err != nil {
		if strings.HasPrefix(rawRepoURL, "git@") {
			return AuthError{http.StatusBadRequest, "malformed clone URL; use ssh:// scheme"}
		} else {
			return AuthError{http.StatusBadRequest, "malformed clone URL"}
		}
	}
	if !slices.Contains(repoURLSchemeAllowlist, repoURL.Scheme) {
		return AuthError{
			http.StatusUnauthorized,
			fmt.Sprintf("clone URL scheme not in allowlist %v",
				repoURLSchemeAllowlist),
		}
	}

	if auth.repoURLs == nil {
		return nil // any
	}

	rawRepoURL = strings.ToLower(rawRepoURL)

	if config.Limits.AllowedRepositoryURLPrefixes != nil {
		allowedPrefix := false
		for _, allowedRepoURLPrefix := range config.Limits.AllowedRepositoryURLPrefixes {
			if strings.HasPrefix(rawRepoURL, strings.ToLower(allowedRepoURLPrefix)) {
				allowedPrefix = true
				break
			}
		}
		if !allowedPrefix {
			return AuthError{
				http.StatusUnauthorized,
				fmt.Sprintf("clone URL not in prefix allowlist %v",
					config.Limits.AllowedRepositoryURLPrefixes),
			}
		}
	}

	allowed := false
	for _, allowedRepoURL := range auth.repoURLs {
		if rawRepoURL == strings.ToLower(allowedRepoURL) {
			allowed = true
			break
		}
	}
	if !allowed {
		return AuthError{
			http.StatusUnauthorized,
			fmt.Sprintf("clone URL not in allowlist %v", auth.repoURLs),
		}
	}

	return nil
}

// The purpose of `allowRepoURLs` is to make sure that only authorized content is deployed
// to the site despite the fact that the non-shared-secret authorization methods allow anyone
// to impersonate the legitimate webhook sender. (If switching to another repository URL would
// be catastrophic, then so would be switching to a different branch.)
func AuthorizeBranch(branch string, auth *Authorization) error {
	if auth.repoURLs == nil {
		return nil // any
	}

	if branch == auth.branch {
		return nil
	} else {
		return AuthError{
			http.StatusUnauthorized,
			fmt.Sprintf("branch %s not in allowlist %v", branch, []string{auth.branch}),
		}
	}
}

func AuthorizeUpdateFromArchive(r *http.Request) (*Authorization, error) {
	causes := []error{AuthError{http.StatusUnauthorized, "unauthorized"}}

	if err := CheckForbiddenDomain(r); err != nil {
		return nil, err
	}

	auth := authorizeInsecure()
	if auth != nil {
		return auth, nil
	}

	if config.Limits.AllowedRepositoryURLPrefixes != nil {
		return nil, AuthError{http.StatusUnauthorized, "updating from archive not allowed"}
	}

	// DNS challenge gives absolute authority.
	auth, err := authorizeDNSChallenge(r)
	if err != nil && IsUnauthorized(err) {
		causes = append(causes, err)
	} else if err != nil { // bad request
		return nil, err
	} else {
		log.Println("auth: DNS challenge")
		return auth, nil
	}

	return nil, joinErrors(causes...)
}

func CheckForbiddenDomain(r *http.Request) error {
	host, err := GetHost(r)
	if err != nil {
		return err
	}

	host = strings.ToLower(host)
	for _, reservedDomain := range config.Limits.ForbiddenDomains {
		reservedDomain = strings.ToLower(reservedDomain)
		if host == reservedDomain || strings.HasSuffix(host, fmt.Sprintf(".%s", reservedDomain)) {
			return AuthError{http.StatusForbidden, "forbidden domain"}
		}
	}

	return nil
}
