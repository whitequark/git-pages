package main

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
)

func GetHost(r *http.Request) string {
	// FIXME: handle IDNA
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		// dirty but the go stdlib doesn't have a "split port if present" function
		host = r.Host
	}
	return host
}

func Authorize(w http.ResponseWriter, r *http.Request) error {
	host := GetHost(r)

	authorization := r.Header.Get("Authorization")
	if authorization == "" {
		http.Error(w, "missing Authorization header", http.StatusUnauthorized)
		return fmt.Errorf("missing Authorization header")
	}

	scheme, param, success := strings.Cut(authorization, " ")
	if !success {
		http.Error(w, "malformed Authorization header", http.StatusBadRequest)
		return fmt.Errorf("malformed Authorization header")
	}

	if scheme != "Pages" {
		http.Error(w, "unknown Authorization scheme", http.StatusBadRequest)
		return fmt.Errorf("unknown Authorization scheme")
	}

	challengeHostname := fmt.Sprintf("_git-pages-challenge.%s", host)
	actualChallenges, err := net.LookupTXT(challengeHostname)
	if err != nil {
		http.Error(w, "failed to look up DNS challenge", http.StatusUnauthorized)
		return fmt.Errorf("failed to look up %s: %s", challengeHostname, err)
	}

	expectedChallenge := fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "%s %s", host, param)))
	if !slices.Contains(actualChallenges, expectedChallenge) {
		http.Error(w,
			fmt.Sprintf("defeated by DNS challenge (%s not in %s)", expectedChallenge, challengeHostname),
			http.StatusUnauthorized,
		)
		return fmt.Errorf(
			"challenge mismatch for %s: %s does not contain %s",
			challengeHostname,
			actualChallenges,
			expectedChallenge,
		)
	}

	return nil
}
