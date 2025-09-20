package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

const notFoundPage = "404.html"
const updateTimeout = 60 * time.Second

func makeWebRoot(host string, projectName string) string {
	return fmt.Sprintf("%s/%s", strings.ToLower(host), projectName)
}

func getPage(w http.ResponseWriter, r *http.Request) error {
	var err error
	var sitePath string
	var manifest *Manifest

	host, err := GetHost(r)
	if err != nil {
		return err
	}

	// allow JavaScript code to access responses (including errors) even across origins
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Max-Age", "86400")

	sitePath, _ = strings.CutPrefix(r.URL.Path, "/")
	if projectName, projectPath, found := strings.Cut(sitePath, "/"); found {
		projectManifest, err := backend.GetManifest(makeWebRoot(host, projectName))
		if err == nil {
			sitePath, manifest = projectPath, projectManifest
		}
	}
	if manifest == nil {
		manifest, err = backend.GetManifest(makeWebRoot(host, ".index"))
		if manifest == nil {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "site not found\n")
			return err
		}
	}

	if sitePath == ".git-pages" {
		// metadata directory name shouldn't be served even if present in site manifest
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "not found\n")
		return nil
	}
	if metadataPath, found := strings.CutPrefix(sitePath, ".git-pages/"); found {
		// metadata requests require authorization to avoid making pushes from private
		// repositories enumerable
		_, err := AuthorizeMetadataRetrieval(r)
		if err != nil {
			return err
		}

		switch metadataPath {
		case "manifest.json":
			w.Header().Add("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(ManifestDebugJSON(manifest)))
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "not found\n")
		}
		return nil
	}

	entryPath := sitePath
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
		entry = manifest.Contents[entryPath]
		if entry == nil || entry.GetType() == Type_Invalid {
			is404 = true
			if entryPath == notFoundPage {
				break
			}
			entryPath = notFoundPage
			continue
		} else if entry.GetType() == Type_InlineFile {
			reader = bytes.NewReader(entry.Data)
		} else if entry.GetType() == Type_ExternalFile {
			etag := fmt.Sprintf(`"%s"`, entry.Data)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return nil
			} else {
				reader, mtime, err = backend.GetBlob(string(entry.Data))
				if err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprintf(w, "internal server error: %s\n", err)
					return err
				}
				w.Header().Set("ETag", etag)
			}
		} else if entry.GetType() == Type_Directory {
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
		} else if entry.GetType() == Type_Symlink {
			return fmt.Errorf("unexpected symlink")
		}
		break
	}

	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
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
		http.ServeContent(w, r, sitePath, mtime, reader)
	}
	return nil
}

const SiteSizeMax = 512 * 1048576

func putPage(w http.ResponseWriter, r *http.Request) error {
	var result UpdateResult

	host, err := GetHost(r)
	if err != nil {
		return err
	}

	projectName, err := GetProjectName(r)
	if err != nil {
		return err
	}

	webRoot := makeWebRoot(host, projectName)

	contentType := r.Header.Get("Content-Type")
	if contentType == "application/x-www-form-urlencoded" {
		auth, err := AuthorizeUpdateFromRepository(r)
		if err != nil {
			return err
		}

		// URLs have no length limit, but 64K seems enough for a repository URL
		requestBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
		if err != nil {
			return fmt.Errorf("body read: %w", err)
		}

		repoURL := string(requestBody)
		if err := AuthorizeRepository(repoURL, auth); err != nil {
			return err
		}

		branch := "pages"
		if customBranch := r.Header.Get("X-Pages-Branch"); customBranch != "" {
			branch = customBranch
		}
		if err := AuthorizeBranch(branch, auth); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(r.Context(), updateTimeout)
		defer cancel()
		result = UpdateFromRepository(ctx, webRoot, repoURL, branch)
	} else {
		_, err := AuthorizeUpdateFromArchive(r)
		if err != nil {
			return err
		}

		// request body contains archive
		reader := http.MaxBytesReader(w, r.Body, SiteSizeMax)
		result = UpdateFromArchive(webRoot, contentType, reader)
	}

	switch result.outcome {
	case UpdateError:
		if errors.Is(result.err, errManifestTooLarge) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		} else if errors.Is(result.err, errArchiveFormat) {
			w.WriteHeader(http.StatusUnsupportedMediaType)
		} else if errors.Is(result.err, errZipBomb) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	case UpdateTimeout:
		w.WriteHeader(http.StatusGatewayTimeout)
	case UpdateNoChange:
		w.Header().Add("X-Pages-Outcome", "no-change")
	case UpdateCreated:
		w.Header().Add("X-Pages-Outcome", "created")
	case UpdateReplaced:
		w.Header().Add("X-Pages-Outcome", "replaced")
	case UpdateDeleted:
		w.Header().Add("X-Pages-Outcome", "deleted")
	}
	if result.manifest != nil {
		if result.manifest.Commit != nil {
			fmt.Fprintln(w, *result.manifest.Commit)
		}
	} else if result.err != nil {
		fmt.Fprintln(w, result.err)
	} else {
		fmt.Fprintln(w, "internal error")
	}
	return nil
}

func deletePage(w http.ResponseWriter, r *http.Request) error {
	_, err := AuthorizeUpdateFromRepository(r)
	if err != nil {
		return err
	}

	host, err := GetHost(r)
	if err != nil {
		return err
	}

	projectName, err := GetProjectName(r)
	if err != nil {
		return err
	}

	err = backend.DeleteManifest(makeWebRoot(host, projectName))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if err != nil {
		fmt.Fprintln(w, err)
	}
	return err
}

func postPage(w http.ResponseWriter, r *http.Request) error {
	auth, err := AuthorizeUpdateFromRepository(r)
	if err != nil {
		return err
	}

	host, err := GetHost(r)
	if err != nil {
		return err
	}

	projectName, err := GetProjectName(r)
	if err != nil {
		return err
	}

	webRoot := makeWebRoot(host, projectName)

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

	// Event payloads have no length limit, but events bigger than 16M seem excessive.
	requestBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16*1048576))
	if err != nil {
		return fmt.Errorf("body read: %w", err)
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
		fmt.Fprintf(w, "ignored %s\n", eventRef)
		return nil
	}

	repoURL := event["repository"].(map[string]any)["clone_url"].(string)
	if err := AuthorizeRepository(repoURL, auth); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(r.Context(), updateTimeout)
	defer cancel()
	result := UpdateFromRepository(ctx, webRoot, repoURL, "pages")
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
	case UpdateDeleted:
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "deleted")
	}
	return nil
}

func ServePages(w http.ResponseWriter, r *http.Request) {
	log.Println("pages:", r.Method, r.Host, r.URL, r.Header.Get("Content-Type"))
	if region := os.Getenv("FLY_REGION"); region != "" {
		w.Header().Add("Server", fmt.Sprintf("git-pages (fly.io; %s)", region))
	} else {
		w.Header().Add("Server", "git-pages")
	}
	err := error(nil)
	switch r.Method {
	// REST API
	case http.MethodGet, http.MethodHead:
		err = getPage(w, r)
	case http.MethodPut:
		err = putPage(w, r)
	case http.MethodDelete:
		err = deletePage(w, r)
	// webhook API
	case http.MethodPost:
		err = postPage(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		err = fmt.Errorf("method %s not allowed", r.Method)
	}
	if err != nil {
		var authErr AuthError
		if errors.As(err, &authErr) {
			message := fmt.Sprint(err)
			http.Error(w, strings.ReplaceAll(message, "\n", "\n- "), authErr.code)
			err = errors.New(strings.ReplaceAll(message, "\n", "; "))
		}
		log.Println("pages err:", err)
	}
}
