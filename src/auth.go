package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"os"
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

	if os.Getenv("INSECURE") != "" {
		return nil // for testing only
	}

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

	if scheme != "Pages" && scheme != "Basic" {
		http.Error(w, "unknown Authorization scheme", http.StatusBadRequest)
		return fmt.Errorf("unknown Authorization scheme")
	}

	// services like GitHub and Gogs cannot send a custom Authorization: header, but supplying
	// username and password in the URL is basically just as good
	if scheme == "Basic" {
		basicParam, err := base64.StdEncoding.DecodeString(param)
		if err != nil {
			http.Error(w, "malformed Authorization: Basic header", http.StatusBadRequest)
			return fmt.Errorf("malformed Authorization: Basic header")
		}

		username, password, found := strings.Cut(string(basicParam), ":")
		if !found {
			http.Error(w, "malformed Authorization: Basic parameter", http.StatusBadRequest)
			return fmt.Errorf("malformed Authorization: Basic parameter")
		}

		if username != "Pages" {
			http.Error(w, "unexpected Authorization: Basic username", http.StatusUnauthorized)
			return fmt.Errorf("unexpected Authorization: Basic username")
		}

		param = password
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
