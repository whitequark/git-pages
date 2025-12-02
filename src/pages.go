package git_pages

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/pquerna/cachecontrol/cacheobject"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const notFoundPage = "404.html"

var (
	serveEncodingCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "git_pages_serve_encoding_count",
		Help: "Count of blob transform vs negotiated encoding",
	}, []string{"transform", "negotiated"})

	siteUpdatesCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "git_pages_site_updates",
		Help: "Count of site updates in total",
	}, []string{"via"})
	siteUpdateOkCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "git_pages_site_update_ok",
		Help: "Count of successful site updates",
	}, []string{"outcome"})
	siteUpdateErrorCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "git_pages_site_update_error",
		Help: "Count of failed site updates",
	}, []string{"cause"})
)

func reportSiteUpdate(via string, result *UpdateResult) {
	siteUpdatesCount.With(prometheus.Labels{"via": via}).Inc()

	switch result.outcome {
	case UpdateError:
		siteUpdateErrorCount.With(prometheus.Labels{"cause": "other"}).Inc()
	case UpdateTimeout:
		siteUpdateErrorCount.With(prometheus.Labels{"cause": "timeout"}).Inc()
	case UpdateNoChange:
		siteUpdateOkCount.With(prometheus.Labels{"outcome": "no-change"}).Inc()
	case UpdateCreated:
		siteUpdateOkCount.With(prometheus.Labels{"outcome": "created"}).Inc()
	case UpdateReplaced:
		siteUpdateOkCount.With(prometheus.Labels{"outcome": "replaced"}).Inc()
	case UpdateDeleted:
		siteUpdateOkCount.With(prometheus.Labels{"outcome": "deleted"}).Inc()
	}
}

func makeWebRoot(host string, projectName string) string {
	return fmt.Sprintf("%s/%s", strings.ToLower(host), projectName)
}

func writeRedirect(w http.ResponseWriter, code int, path string) {
	w.Header().Set("Location", path)
	w.WriteHeader(code)
	fmt.Fprintf(w, "see %s\n", path)
}

// The `clauspost/compress/zstd` package recommends reusing a decompressor to avoid repeated
// allocations of internal buffers.
var zstdDecoder, _ = zstd.NewReader(nil)

func getPage(w http.ResponseWriter, r *http.Request) error {
	var err error
	var sitePath string
	var manifest *Manifest
	var manifestMtime time.Time

	cacheControl, err := cacheobject.ParseRequestCacheControl(r.Header.Get("Cache-Control"))
	if err != nil {
		cacheControl = &cacheobject.RequestCacheDirectives{
			MaxAge:   -1,
			MaxStale: -1,
			MinFresh: -1,
		}
	}

	bypassCache := cacheControl.NoCache || cacheControl.MaxAge == 0

	host, err := GetHost(r)
	if err != nil {
		return err
	}

	type indexManifestResult struct {
		manifest      *Manifest
		manifestMtime time.Time
		err           error
	}
	indexManifestCh := make(chan indexManifestResult, 1)
	go func() {
		manifest, mtime, err := backend.GetManifest(
			r.Context(), makeWebRoot(host, ".index"),
			GetManifestOptions{BypassCache: bypassCache},
		)
		indexManifestCh <- (indexManifestResult{manifest, mtime, err})
	}()

	err = nil
	sitePath = strings.TrimPrefix(r.URL.Path, "/")
	if projectName, projectPath, hasProjectSlash := strings.Cut(sitePath, "/"); projectName != "" {
		var projectManifest *Manifest
		var projectManifestMtime time.Time
		projectManifest, projectManifestMtime, err = backend.GetManifest(
			r.Context(), makeWebRoot(host, projectName),
			GetManifestOptions{BypassCache: bypassCache},
		)
		if err == nil {
			if !hasProjectSlash {
				writeRedirect(w, http.StatusFound, r.URL.Path+"/")
				return nil
			}
			sitePath, manifest, manifestMtime = projectPath, projectManifest, projectManifestMtime
		}
	}
	if manifest == nil && (err == nil || errors.Is(err, ErrObjectNotFound)) {
		result := <-indexManifestCh
		manifest, manifestMtime, err = result.manifest, result.manifestMtime, result.err
		if manifest == nil && errors.Is(err, ErrObjectNotFound) {
			if found, fallbackErr := HandleWildcardFallback(w, r); found {
				return fallbackErr
			} else {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, "site not found\n")
				return err
			}
		}
	}
	if err != nil {
		ObserveError(err) // all storage errors must be reported
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "internal server error (%s)\n", err)
		return err
	}

	if r.Header.Get("Origin") != "" {
		// allow JavaScript code to access responses (including errors) even across origins
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}

	if sitePath == ".git-pages" {
		// metadata directory name shouldn't be served even if present in site manifest
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "not found\n")
		return nil
	}
	if metadataPath, found := strings.CutPrefix(sitePath, ".git-pages/"); found {
		lastModified := manifestMtime.UTC().Format(http.TimeFormat)
		switch {
		case metadataPath == "health":
			w.Header().Add("Last-Modified", lastModified)
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "ok\n")
			return nil

		case metadataPath == "manifest.json":
			// metadata requests require authorization to avoid making pushes from private
			// repositories enumerable
			_, err := AuthorizeMetadataRetrieval(r)
			if err != nil {
				return err
			}

			w.Header().Add("Content-Type", "application/json; charset=utf-8")
			w.Header().Add("Last-Modified", lastModified)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(ManifestDebugJSON(manifest)))
			return nil

		case metadataPath == "archive.tar" && config.Feature("archive-site"):
			// same as above
			_, err := AuthorizeMetadataRetrieval(r)
			if err != nil {
				return err
			}

			// we only offer `/.git-pages/archive.tar` and not the `.tar.gz`/`.tar.zst` variants
			// because HTTP can already request compression using the `Content-Encoding` mechanism
			acceptedEncodings := parseHTTPEncodings(r.Header.Get("Accept-Encoding"))
			negotiated := acceptedEncodings.Negotiate("zstd", "gzip", "identity")
			if negotiated != "" {
				w.Header().Set("Content-Encoding", negotiated)
			}
			w.Header().Add("Content-Type", "application/x-tar")
			w.Header().Add("Last-Modified", lastModified)
			w.Header().Add("Transfer-Encoding", "chunked")
			w.WriteHeader(http.StatusOK)
			var iow io.Writer
			switch negotiated {
			case "", "identity":
				iow = w
			case "gzip":
				iow = gzip.NewWriter(w)
			case "zstd":
				iow, _ = zstd.NewWriter(w)
			}
			return CollectTar(r.Context(), iow, manifest, manifestMtime)

		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "not found\n")
			return nil
		}
	}

	entryPath := sitePath
	entry := (*Entry)(nil)
	appliedRedirect := false
	status := 200
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
		if !appliedRedirect {
			redirectKind := RedirectAny
			if entry != nil && entry.GetType() != Type_Invalid {
				redirectKind = RedirectForce
			}
			originalURL := (&url.URL{Host: r.Host}).ResolveReference(r.URL)
			redirectURL, redirectStatus := ApplyRedirectRules(manifest, originalURL, redirectKind)
			if Is3xxHTTPStatus(redirectStatus) {
				writeRedirect(w, redirectStatus, redirectURL.String())
				return nil
			} else if redirectURL != nil {
				entryPath = strings.TrimPrefix(redirectURL.Path, "/")
				status = int(redirectStatus)
				// Apply user redirects at most once; if something ends in a loop, it should be
				// the user agent, not the pages server.
				appliedRedirect = true
				continue
			}
		}
		if entry == nil || entry.GetType() == Type_Invalid {
			status = 404
			if entryPath != notFoundPage {
				entryPath = notFoundPage
				continue
			} else {
				reader = bytes.NewReader([]byte("not found\n"))
				break
			}
		} else if entry.GetType() == Type_InlineFile {
			reader = bytes.NewReader(entry.Data)
		} else if entry.GetType() == Type_ExternalFile {
			etag := fmt.Sprintf(`"%s"`, entry.Data)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return nil
			} else {
				reader, _, mtime, err = backend.GetBlob(r.Context(), string(entry.Data))
				if err != nil {
					ObserveError(err) // all storage errors must be reported
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
				writeRedirect(w, http.StatusFound, newPath)
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

	acceptedEncodings := parseHTTPEncodings(r.Header.Get("Accept-Encoding"))
	negotiatedEncoding := true
	switch entry.GetTransform() {
	case Transform_Identity:
		switch acceptedEncodings.Negotiate("identity") {
		case "identity":
			serveEncodingCount.
				With(prometheus.Labels{"transform": "identity", "negotiated": "identity"}).
				Inc()
		default:
			negotiatedEncoding = false
			serveEncodingCount.
				With(prometheus.Labels{"transform": "identity", "negotiated": "failure"}).
				Inc()
		}
	case Transform_Zstd:
		supported := []string{"zstd", "identity"}
		if entry.ContentType == nil {
			// If Content-Type is unset, `http.ServeContent` will try to sniff
			// the file contents. That won't work if it's compressed.
			supported = []string{"identity"}
		}
		switch acceptedEncodings.Negotiate(supported...) {
		case "zstd":
			// Set Content-Length ourselves since `http.ServeContent` only sets
			// it if Content-Encoding is unset or if it's a range request.
			w.Header().Set("Content-Length", strconv.FormatInt(entry.GetCompressedSize(), 10))
			w.Header().Set("Content-Encoding", "zstd")
			serveEncodingCount.
				With(prometheus.Labels{"transform": "zstd", "negotiated": "zstd"}).
				Inc()
		case "identity":
			compressedData, _ := io.ReadAll(reader)
			decompressedData, err := zstdDecoder.DecodeAll(compressedData, []byte{})
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, "internal server error: %s\n", err)
				return err
			}
			reader = bytes.NewReader(decompressedData)
			serveEncodingCount.
				With(prometheus.Labels{"transform": "zstd", "negotiated": "identity"}).
				Inc()
		default:
			negotiatedEncoding = false
			serveEncodingCount.
				With(prometheus.Labels{"transform": "zstd", "negotiated": "failure"}).
				Inc()
		}
	default:
		return fmt.Errorf("unexpected transform")
	}
	if !negotiatedEncoding {
		w.WriteHeader(http.StatusNotAcceptable)
		return fmt.Errorf("no supported content encodings (accept-encoding: %q)",
			r.Header.Get("Accept-Encoding"))
	}

	if entry != nil && entry.ContentType != nil {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Type", *entry.ContentType)
	}

	customHeaders, err := ApplyHeaderRules(manifest, &url.URL{Path: entryPath})
	if err != nil {
		// This is an "internal server error" from an HTTP point of view, but also
		// either an issue with the site or a misconfiguration from our point of view.
		// Since it's not a problem with the server we don't observe the error.
		//
		// Note that this behavior is different from a site upload with a malformed
		// `_headers` file (where it is semantically ignored); this is because a broken
		// upload is something the uploader can notice and fix, but a change in server
		// configuration is something they are unaware of and won't be notified of.
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "%s\n", err)
		return err
	} else {
		// If the header has passed all of our stringent, deny-by-default checks, it means
		// it's good enough to overwrite whatever was our builtin option (if any).
		maps.Copy(w.Header(), customHeaders)
	}

	// decide on the HTTP status
	if status != 200 {
		w.WriteHeader(status)
		if reader != nil {
			io.Copy(w, reader)
		}
	} else {
		// consider content fresh for 60 seconds (the same as the freshness interval of
		// manifests in the S3 backend), and use stale content anyway as long as it's not
		// older than a hour; while it is cheap to handle If-Modified-Since queries
		// server-side, on the client `max-age=0, must-revalidate` causes every resource
		// to block the page load every time
		w.Header().Set("Cache-Control", "max-age=60, stale-while-revalidate=3600")
		// see https://web.dev/articles/stale-while-revalidate for details

		// http.ServeContent handles conditional requests and range requests
		http.ServeContent(w, r, entryPath, mtime, reader)
	}
	return nil
}

func checkDryRun(w http.ResponseWriter, r *http.Request) bool {
	// "Dry run" requests are used to non-destructively check if the request would have
	// successfully been authorized.
	if r.Header.Get("Dry-Run") != "" {
		fmt.Fprintln(w, "dry-run ok")
		return true
	}
	return false
}

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

	updateCtx, cancel := context.WithTimeout(r.Context(), time.Duration(config.Limits.UpdateTimeout))
	defer cancel()

	contentType := getMediaType(r.Header.Get("Content-Type"))
	switch contentType {
	case "application/x-www-form-urlencoded":
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
		if customBranch := r.Header.Get("Branch"); customBranch != "" {
			branch = customBranch
		}
		if err := AuthorizeBranch(branch, auth); err != nil {
			return err
		}

		if checkDryRun(w, r) {
			return nil
		}

		result = UpdateFromRepository(updateCtx, webRoot, repoURL, branch)

	default:
		_, err := AuthorizeUpdateFromArchive(r)
		if err != nil {
			return err
		}

		if checkDryRun(w, r) {
			return nil
		}

		// request body contains archive
		reader := http.MaxBytesReader(w, r.Body, int64(config.Limits.MaxSiteSize.Bytes()))
		result = UpdateFromArchive(updateCtx, webRoot, contentType, reader)
	}

	switch result.outcome {
	case UpdateError:
		if errors.Is(result.err, ErrManifestTooLarge) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		} else if errors.Is(result.err, errArchiveFormat) {
			w.WriteHeader(http.StatusUnsupportedMediaType)
		} else if errors.Is(result.err, ErrArchiveTooLarge) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	case UpdateTimeout:
		w.WriteHeader(http.StatusGatewayTimeout)
	case UpdateNoChange:
		w.Header().Add("Update-Result", "no-change")
	case UpdateCreated:
		w.Header().Add("Update-Result", "created")
	case UpdateReplaced:
		w.Header().Add("Update-Result", "replaced")
	case UpdateDeleted:
		w.Header().Add("Update-Result", "deleted")
	}
	if result.manifest != nil {
		if result.manifest.Commit != nil {
			fmt.Fprintln(w, *result.manifest.Commit)
		} else {
			fmt.Fprintln(w, "(archive)")
		}
		for _, problem := range GetProblemReport(result.manifest) {
			fmt.Fprintln(w, problem)
		}
	} else if result.err != nil {
		fmt.Fprintln(w, result.err)
	} else {
		fmt.Fprintln(w, "internal error")
	}
	reportSiteUpdate("rest", &result)
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

	if checkDryRun(w, r) {
		return nil
	}

	err = backend.DeleteManifest(r.Context(), makeWebRoot(host, projectName))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	} else {
		w.Header().Add("Update-Result", "deleted")
		w.WriteHeader(http.StatusOK)
	}
	if err != nil {
		fmt.Fprintln(w, err)
	}
	return err
}

func postPage(w http.ResponseWriter, r *http.Request) error {
	// Start a timer for the request timeout immediately.
	requestTimeout := 3 * time.Second
	requestTimer := time.NewTimer(requestTimeout)

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

	var event struct {
		Ref        string `json:"ref"`
		Repository struct {
			CloneURL string `json:"clone_url"`
		} `json:"repository"`
	}
	err = json.NewDecoder(bytes.NewReader(requestBody)).Decode(&event)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %s", err), http.StatusBadRequest)
		return err
	}

	if event.Ref != fmt.Sprintf("refs/heads/%s", auth.branch) {
		code := http.StatusUnauthorized
		if strings.Contains(r.Header.Get("User-Agent"), "GitHub-Hookshot") {
			// GitHub has no way to restrict branches for a webhook, and responding with 401
			// for every non-pages branch makes the "Recent Deliveries" tab look awful.
			code = http.StatusOK
		}
		http.Error(w,
			fmt.Sprintf("ref %s not in allowlist [refs/heads/%v]", event.Ref, auth.branch),
			code)
		return nil
	}

	repoURL := event.Repository.CloneURL
	if err := AuthorizeRepository(repoURL, auth); err != nil {
		return err
	}

	if checkDryRun(w, r) {
		return nil
	}

	resultChan := make(chan UpdateResult)
	go func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, time.Duration(config.Limits.UpdateTimeout))
		defer cancel()

		result := UpdateFromRepository(ctx, webRoot, repoURL, auth.branch)
		resultChan <- result
		reportSiteUpdate("webhook", &result)
	}(context.Background())

	var result UpdateResult
	select {
	case result = <-resultChan:
	case <-requestTimer.C:
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "updating (taking longer than %s)", requestTimeout)
		return nil
	}

	switch result.outcome {
	case UpdateError:
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "update error: %s\n", result.err)
	case UpdateTimeout:
		w.WriteHeader(http.StatusGatewayTimeout)
		fmt.Fprintln(w, "update timeout")
	case UpdateNoChange:
		fmt.Fprintln(w, "unchanged")
	case UpdateCreated:
		fmt.Fprintln(w, "created")
	case UpdateReplaced:
		fmt.Fprintln(w, "replaced")
	case UpdateDeleted:
		fmt.Fprintln(w, "deleted")
	}
	if result.manifest != nil {
		report := GetProblemReport(result.manifest)
		if len(report) > 0 {
			fmt.Fprintln(w, "problems:")
		}
		for _, problem := range report {
			fmt.Fprintf(w, "- %s\n", problem)
		}
	}
	return nil
}

func ServePages(w http.ResponseWriter, r *http.Request) {
	// We want upstream health checks to be done as closely to the normal flow as possible;
	// any intentional deviation is an opportunity to miss an issue that will affect our
	// visitors but not our health checks.
	if r.Header.Get("Health-Check") == "" {
		logc.Println(r.Context(), "pages:", r.Method, r.Host, r.URL, r.Header.Get("Content-Type"))
		if region := os.Getenv("FLY_REGION"); region != "" {
			machine_id := os.Getenv("FLY_MACHINE_ID")
			w.Header().Add("Server", fmt.Sprintf("git-pages (fly.io; %s; %s)", region, machine_id))
			ObserveData(r.Context(), "server.name", machine_id, "server.region", region)
		} else if hostname, err := os.Hostname(); err == nil {
			if region := os.Getenv("PAGES_REGION"); region != "" {
				w.Header().Add("Server", fmt.Sprintf("git-pages (%s; %s)", region, hostname))
				ObserveData(r.Context(), "server.name", hostname, "server.region", region)
			} else {
				w.Header().Add("Server", fmt.Sprintf("git-pages (%s)", hostname))
				ObserveData(r.Context(), "server.name", hostname)
			}
		}
	}
	allowedMethods := []string{"OPTIONS", "HEAD", "GET", "PUT", "DELETE", "POST"}
	if r.Method == "OPTIONS" || !slices.Contains(allowedMethods, r.Method) {
		w.Header().Add("Allow", strings.Join(allowedMethods, ", "))
	}
	err := error(nil)
	switch r.Method {
	// REST API
	case http.MethodOptions:
		// no preflight options
	case http.MethodHead, http.MethodGet:
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
			http.Error(w, prettyErrMsg(err), authErr.code)
		}
		var tooLargeErr *http.MaxBytesError
		if errors.As(err, &tooLargeErr) {
			message := "request body too large"
			http.Error(w, message, http.StatusRequestEntityTooLarge)
			err = errors.New(message)
		}
		logc.Println(r.Context(), "pages err:", err)
	}
}
