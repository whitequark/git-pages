package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	securejoin "github.com/cyphar/filepath-securejoin"
	"golang.org/x/sys/unix"
)

const fetchTimeout = 30 * time.Second

func getPage(w http.ResponseWriter, r *http.Request) error {
	host := GetHost(r)

	// if the first directory of the path exists under `www/$host`, use it as the root,
	// else use `www/$host/.index`
	path, _ := strings.CutPrefix(r.URL.Path, "/")
	wwwRoot := filepath.Join("www", host, ".index")
	requestPath := path
	if projectName, projectPath, found := strings.Cut(path, "/"); found {
		projectRoot := filepath.Join("www", host, projectName)
		if file, _ := securejoin.OpenInRoot(config.DataDir, projectRoot); file != nil {
			file.Close()
			wwwRoot, requestPath = projectRoot, projectPath
		}
	}

	// try to serve `$root/$path` first
	file, err := securejoin.OpenInRoot(config.DataDir, filepath.Join(wwwRoot, requestPath))
	if err == nil {
		// if it's a directory, serve `$root/$path/index.html`
		stat, statErr := file.Stat()
		if statErr == nil && stat.IsDir() {
			defer file.Close()
			file, err = securejoin.OpenInRoot(config.DataDir,
				filepath.Join(wwwRoot, requestPath, "index.html"))
		}
	}

	// if whatever we were serving doesn't exist, try to serve `$root/404.html`
	if errors.Is(err, os.ErrNotExist) {
		file, _ = securejoin.OpenInRoot(config.DataDir, filepath.Join(wwwRoot, "404.html"))
	}

	// acquire read capability to the file being served (if possible)
	reader := io.ReadSeeker(nil)
	if file != nil {
		defer file.Close()
		file, err = securejoin.Reopen(file, unix.O_RDONLY)
		if file != nil {
			defer file.Close()
			reader = file
		}
	}

	// decide on the HTTP status
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			w.WriteHeader(http.StatusNotFound)
			if reader == nil {
				reader = bytes.NewReader([]byte("not found\n"))
			}
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			reader = bytes.NewReader([]byte("internal server error\n"))
		}
		// serve custom 404 page (if any)
		io.Copy(w, reader)
	} else {
		// always check whether content has changed with the origin server; it is cheap to handle
		// ETag or If-Modified-Since queries and it avoids stale content being served
		w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")

		// `www/$host` should be a symlink pointing to an immutable directory under `tree/...`;
		// if it's not, assume the server administrator did it on purpose and degrade gracefully
		wwwRootDest, err := os.Readlink(filepath.Join(config.DataDir, wwwRoot))
		if err == nil {
			w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha1.Sum([]byte(wwwRootDest))))
		}

		// http.ServeContent handles content type and caching
		stat, _ := file.Stat()
		http.ServeContent(w, r, path, stat.ModTime(), reader)
	}

	return err
}

func getProjectName(w http.ResponseWriter, r *http.Request) (string, error) {
	// path must be either `/` or `/foo/` (`/foo` is accepted as an alias)
	path, _ := strings.CutPrefix(r.URL.Path, "/")
	path, _ = strings.CutSuffix(path, "/")
	if strings.HasPrefix(path, ".") {
		http.Error(w, "this directory name is reserved for system use", http.StatusBadRequest)
		return "", fmt.Errorf("reserved name")
	} else if strings.Contains(path, "/") {
		http.Error(w, "only one level of nesting is allowed", http.StatusBadRequest)
		return "", fmt.Errorf("nesting too deep")
	}

	if path == "" {
		// path `/` corresponds to pseudo-project `.index`
		return ".index", nil
	} else {
		return path, nil
	}
}

func putPage(w http.ResponseWriter, r *http.Request) error {
	host := GetHost(r)

	err := Authorize(w, r)
	if err != nil {
		return err
	}

	projectName, err := getProjectName(w, r)
	if err != nil {
		return err
	}

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("body read: %s", err)
	}

	// request body contains git repository URL `https://codeberg.org/...`
	// request header X-Pages-Branch contains git branch, `pages` by default
	webRoot := fmt.Sprintf("%s/%s", host, projectName)
	repoURL := string(requestBody)
	branch := r.Header.Get("X-Pages-Branch")
	if branch == "" {
		branch = "pages"
	}

	result := FetchWithTimeout(webRoot, repoURL, branch, fetchTimeout)
	if result.err == nil {
		w.Header().Add("Content-Location", r.URL.String())
	}
	switch result.outcome {
	case FetchError:
		w.WriteHeader(http.StatusServiceUnavailable)
	case FetchTimeout:
		w.WriteHeader(http.StatusGatewayTimeout)
		// HTTP prescribes these response codes to be used
	case FetchNoChange:
		w.WriteHeader(http.StatusNoContent)
	case FetchCreated:
		w.WriteHeader(http.StatusCreated)
	case FetchUpdated:
		w.WriteHeader(http.StatusOK)
	}
	if result.err != nil {
		fmt.Fprintln(w, result.err)
	} else {
		fmt.Fprintln(w, result.head)
	}
	return result.err
}

func postPage(w http.ResponseWriter, r *http.Request) error {
	host := GetHost(r)
	hostParts := strings.Split(host, ".")

	projectName, err := getProjectName(w, r)
	if err != nil {
		return err
	}

	allowRepoURL := ""
	if slices.Equal(hostParts[1:], strings.Split(config.Wildcard.Domain, ".")) {
		userName := hostParts[0]
		repoName := projectName
		if repoName == ".index" {
			repoName = fmt.Sprintf(config.Wildcard.IndexRepo, userName)
		}
		allowRepoURL = fmt.Sprintf(config.Wildcard.CloneURL, userName, repoName)
	} else {
		// GitHub and Gogs cannot supply an `Authorization:` header.
		if r.Header.Get("Authorization") == "" {
			if value := r.URL.Query().Get("Authorization"); value != "" {
				r.Header.Set("Authorization", value)
			}
		}

		if err := Authorize(w, r); err != nil {
			return err
		}
	}

	eventName := ""
	for _, header := range []string{
		"X-Forgejo-Event",
		"X-GitHub-Event",
		"X-Gitea-Event",
		"X-Gogs-Event",
	} {
		eventName = r.Header.Get(header)
		if eventName != "" {
			break
		}
	}

	if eventName == "" {
		http.Error(w,
			"expected a Forgejo, GitHub, Gitea, or Gogs webhook request", http.StatusBadRequest)
		return fmt.Errorf("event expected")
	}

	if eventName != "push" {
		http.Error(w, "only push events are allowed", http.StatusBadRequest)
		return fmt.Errorf("invalid event")
	}

	if r.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "only JSON payload is allowed", http.StatusBadRequest)
		return fmt.Errorf("invalid content type")
	}

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("body read: %s", err)
	}

	var event map[string]any
	err = json.NewDecoder(bytes.NewReader(requestBody)).Decode(&event)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %s", err), http.StatusBadRequest)
		return err
	}

	eventRef := event["ref"].(string)
	if eventRef != "refs/heads/pages" {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "ref %s ignored\n", eventRef)
		return nil
	}

	webRoot := fmt.Sprintf("%s/%s", host, projectName)

	repoURL := event["repository"].(map[string]any)["clone_url"].(string)
	if allowRepoURL != "" && !strings.EqualFold(repoURL, allowRepoURL) {
		http.Error(w,
			fmt.Sprintf("wildcard domain requires repository to be %s", allowRepoURL),
			http.StatusUnauthorized,
		)
		return fmt.Errorf("invalid clone URL")
	}

	result := FetchWithTimeout(webRoot, repoURL, "pages", fetchTimeout)
	switch result.outcome {
	case FetchError:
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "fetch error: %s\n", result.err)
	case FetchTimeout:
		w.WriteHeader(http.StatusGatewayTimeout)
		fmt.Fprintln(w, "fetch timeout")
	case FetchNoChange:
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "unchanged")
	case FetchCreated:
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "created")
	case FetchUpdated:
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "updated")
	}
	return result.err
}

func ServePages(w http.ResponseWriter, r *http.Request) {
	log.Println("pages:", r.Method, r.Host, r.URL)
	w.Header().Add("Server", "git-pages")
	err := error(nil)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		err = getPage(w, r)
	case http.MethodPut:
		err = putPage(w, r)
	case http.MethodPost:
		err = postPage(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		err = fmt.Errorf("method %s not allowed", r.Method)
	}
	if err != nil {
		if pathErr, ok := err.(*os.PathError); ok {
			err = fmt.Errorf("not found: %s", pathErr.Path)
		}
		log.Println("pages err:", err)
	}
}
