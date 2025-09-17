package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

const notFoundPage = "404.html"
const updateTimeout = 60 * time.Second

func getPage(w http.ResponseWriter, r *http.Request) error {
	var err error
	var urlPath string
	var manifest *Manifest

	host := GetHost(r)

	// allow JavaScript code to access responses (including errors) even across origins
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Max-Age", "86400")

	urlPath, _ = strings.CutPrefix(r.URL.Path, "/")
	if projectName, projectPath, found := strings.Cut(urlPath, "/"); found {
		projectManifest, err := backend.GetManifest(fmt.Sprintf("%s/%s", host, projectName))
		if err == nil {
			urlPath, manifest = projectPath, projectManifest
		}
	}
	if manifest == nil {
		manifest, err = backend.GetManifest(fmt.Sprintf("%s/.index", host))
		if manifest == nil {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "site not found\n")
			return err
		}
	}

	entryPath := urlPath
	entry := (*Entry)(nil)
	is404 := false
	reader := io.ReadSeeker(nil)
	mtime := time.Time{}
	for {
		entryPath, _ = strings.CutSuffix(entryPath, "/")
		entryPath, err = ExpandSymlinks(manifest, entryPath)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(w, err)
			return err
		}
		entry = manifest.Tree[entryPath]
		if entry == nil || entry.Type == Type_Invalid {
			is404 = true
			if entryPath == notFoundPage {
				break
			}
			entryPath = notFoundPage
			continue
		} else if entry.Type == Type_InlineFile {
			reader = bytes.NewReader(entry.Data)
		} else if entry.Type == Type_ExternalFile {
			etag := fmt.Sprintf(`"%x"`, entry.Data)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return nil
			} else {
				reader, mtime, err = backend.GetBlob(string(entry.Data))
				if err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprintf(w, "internal server error\n")
					return err
				}
				w.Header().Set("ETag", etag)
			}
		} else if entry.Type == Type_Directory {
			if strings.HasSuffix(r.URL.Path, "/") {
				entryPath = path.Join(entryPath, "index.html")
				continue
			} else {
				// redirect from `dir` to `dir/`, otherwise when `dir/index.html` is served,
				// links in it will have the wrong base URL
				newPath := r.URL.Path + "/"
				w.Header().Set("Location", newPath)
				w.WriteHeader(http.StatusFound)
				fmt.Fprintf(w, "see %s\n", newPath)
				return nil
			}
		} else if entry.Type == Type_Symlink {
			return fmt.Errorf("unexpected symlink")
		}
		break
	}

	// decide on the HTTP status
	if is404 {
		w.WriteHeader(http.StatusNotFound)
		if entry == nil {
			fmt.Fprintf(w, "not found\n")
		} else {
			io.Copy(w, reader)
		}
	} else {
		// allow the use of multi-threading in WebAssembly
		w.Header().Set("Cross-Origin-Embedder-Policy", "credentialless")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")

		// always check whether content has changed with the origin server; it is cheap to handle
		// ETag or If-Modified-Since queries and it avoids stale content being served
		w.Header().Set("Cache-Control", "public, max-age=0, must-revalidate")

		// http.ServeContent handles content type and caching
		http.ServeContent(w, r, urlPath, mtime, reader)
	}
	return nil
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

	result := UpdateWithTimeout(webRoot, repoURL, branch, updateTimeout)
	if result.manifest != nil {
		w.Header().Add("Content-Location", r.URL.String())
	}
	switch result.outcome {
	case UpdateError:
		w.WriteHeader(http.StatusServiceUnavailable)
	case UpdateTimeout:
		w.WriteHeader(http.StatusGatewayTimeout)
		// HTTP prescribes these response codes to be used
	case UpdateNoChange:
		w.WriteHeader(http.StatusNoContent)
	case UpdateCreated:
		w.WriteHeader(http.StatusCreated)
	case UpdateReplaced:
		w.WriteHeader(http.StatusOK)
	}
	if result.manifest != nil {
		fmt.Fprintln(w, result.manifest.Commit)
	} else if result.err != nil {
		fmt.Fprintln(w, result.err)
	} else {
		fmt.Fprintln(w, "internal error")
	}
	return nil
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

	result := UpdateWithTimeout(webRoot, repoURL, "pages", updateTimeout)
	switch result.outcome {
	case UpdateError:
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "update error: %s\n", result.err)
	case UpdateTimeout:
		w.WriteHeader(http.StatusGatewayTimeout)
		fmt.Fprintln(w, "update timeout")
	case UpdateNoChange:
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "unchanged")
	case UpdateCreated:
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "created")
	case UpdateReplaced:
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "replaced")
	}
	return nil
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
		if minioErr, ok := err.(minio.ErrorResponse); ok && minioErr.Code == "NoSuchKey" {
			err = fmt.Errorf("not found: %s", minioErr.Key)
		}
		log.Println("pages err:", err)
	}
}
