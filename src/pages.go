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
	"google.golang.org/protobuf/proto"
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

func observeSiteUpdate(via string, result *UpdateResult) {
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
	return path.Join(strings.ToLower(host), projectName)
}

func getWebRoot(r *http.Request) (string, error) {
	host, err := GetHost(r)
	if err != nil {
		return "", err
	}

	projectName, err := GetProjectName(r)
	if err != nil {
		return "", err
	}

	return makeWebRoot(host, projectName), nil
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
	var metadata ManifestMetadata

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
		manifest *Manifest
		metadata ManifestMetadata
		err      error
	}
	indexManifestCh := make(chan indexManifestResult, 1)
	go func() {
		manifest, metadata, err := backend.GetManifest(
			r.Context(), makeWebRoot(host, ".index"),
			GetManifestOptions{BypassCache: bypassCache},
		)
		indexManifestCh <- (indexManifestResult{manifest, metadata, err})
	}()

	err = nil
	sitePath = strings.TrimPrefix(r.URL.Path, "/")
	if projectName, projectPath, hasProjectSlash := strings.Cut(sitePath, "/"); projectName != "" {
		if IsValidProjectName(projectName) {
			var projectManifest *Manifest
			var projectMetadata ManifestMetadata
			projectManifest, projectMetadata, err = backend.GetManifest(
				r.Context(), makeWebRoot(host, projectName),
				GetManifestOptions{BypassCache: bypassCache},
			)
			if err == nil {
				if !hasProjectSlash {
					writeRedirect(w, http.StatusFound, r.URL.Path+"/")
					return nil
				}
				sitePath, manifest, metadata = projectPath, projectManifest, projectMetadata
			}
		}
	}
	if manifest == nil && (err == nil || errors.Is(err, ErrObjectNotFound)) {
		result := <-indexManifestCh
		manifest, metadata, err = result.manifest, result.metadata, result.err
		if manifest == nil && errors.Is(err, ErrObjectNotFound) {
			if fallback != nil {
				logc.Printf(r.Context(), "fallback: %s via %s", host, config.Fallback.ProxyTo)
				fallback.ServeHTTP(w, r)
				return nil
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
		lastModified := metadata.LastModified.UTC().Format(http.TimeFormat)
		switch {
		case metadataPath == "health":
			w.Header().Add("Last-Modified", lastModified)
			w.Header().Add("ETag", fmt.Sprintf("\"%s\"", metadata.ETag))
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
			w.Header().Add("ETag", fmt.Sprintf("\"%s-manifest\"", metadata.ETag))
			w.WriteHeader(http.StatusOK)
			w.Write(ManifestJSON(manifest))
			return nil

		case metadataPath == "archive.tar":
			// same as above
			_, err := AuthorizeMetadataRetrieval(r)
			if err != nil {
				return err
			}

			// we only offer `/.git-pages/archive.tar` and not the `.tar.gz`/`.tar.zst` variants
			// because HTTP can already request compression using the `Content-Encoding` mechanism
			acceptedEncodings := ParseAcceptEncodingHeader(r.Header.Get("Accept-Encoding"))
			w.Header().Add("Vary", "Accept-Encoding")
			negotiated := acceptedEncodings.Negotiate("zstd", "gzip", "identity")
			if negotiated != "" {
				w.Header().Set("Content-Encoding", negotiated)
			}
			w.Header().Add("Content-Type", "application/x-tar")
			w.Header().Add("Last-Modified", lastModified)
			w.Header().Add("ETag", fmt.Sprintf("\"%s-archive\"", metadata.ETag))
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
			return CollectTar(r.Context(), iow, manifest, metadata)

		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "not found\n")
			return nil
		}
	}

	entryPath := sitePath
	entry := (*Entry)(nil)
	appliedRedirect := false
	status := http.StatusOK
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
			if entry != nil && entry.GetType() != Type_InvalidEntry {
				redirectKind = RedirectForce
			}
			originalURL := (&url.URL{Host: r.Host}).ResolveReference(r.URL)
			_, redirectURL, redirectStatus := ApplyRedirectRules(manifest, originalURL, redirectKind)
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
		if entry == nil || entry.GetType() == Type_InvalidEntry {
			status = http.StatusNotFound
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
				var metadata BlobMetadata
				reader, metadata, err = backend.GetBlob(r.Context(), string(entry.Data))
				if err != nil {
					ObserveError(err) // all storage errors must be reported
					w.WriteHeader(http.StatusInternalServerError)
					fmt.Fprintf(w, "internal server error: %s\n", err)
					return err
				}
				mtime = metadata.LastModified
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

	var offeredEncodings []string
	acceptedEncodings := ParseAcceptEncodingHeader(r.Header.Get("Accept-Encoding"))
	w.Header().Add("Vary", "Accept-Encoding")
	negotiatedEncoding := true
	switch entry.GetTransform() {
	case Transform_Identity:
		offeredEncodings = []string{"identity"}
		switch acceptedEncodings.Negotiate(offeredEncodings...) {
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
		offeredEncodings = []string{"zstd", "identity"}
		if entry.ContentType == nil {
			// If Content-Type is unset, `http.ServeContent` will try to sniff
			// the file contents. That won't work if it's compressed.
			offeredEncodings = []string{"identity"}
		}
		switch acceptedEncodings.Negotiate(offeredEncodings...) {
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
		w.Header().Set("Accept-Encoding", strings.Join(offeredEncodings, ", "))
		w.WriteHeader(http.StatusNotAcceptable)
		return fmt.Errorf("no supported content encodings (Accept-Encoding: %s)",
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
		if _, hasCacheControl := w.Header()["Cache-Control"]; !hasCacheControl {
			// consider content fresh for 60 seconds (the same as the freshness interval of
			// manifests in the S3 backend), and use stale content anyway as long as it's not
			// older than a hour; while it is cheap to handle If-Modified-Since queries
			// server-side, on the client `max-age=0, must-revalidate` causes every resource
			// to block the page load every time
			w.Header().Set("Cache-Control", "max-age=60, stale-while-revalidate=3600")
			// see https://web.dev/articles/stale-while-revalidate for details
		}

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

	for _, header := range []string{
		"If-Modified-Since", "If-Unmodified-Since", "If-Match", "If-None-Match",
	} {
		if r.Header.Get(header) != "" {
			http.Error(w, fmt.Sprintf("unsupported precondition %s", header), http.StatusBadRequest)
			return nil
		}
	}

	webRoot, err := getWebRoot(r)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(config.Limits.UpdateTimeout))
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

		result = UpdateFromRepository(ctx, webRoot, repoURL, branch)

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
		result = UpdateFromArchive(ctx, webRoot, contentType, reader)
	}

	return reportUpdateResult(w, r, result)
}

func patchPage(w http.ResponseWriter, r *http.Request) error {
	for _, header := range []string{
		"If-Modified-Since", "If-Unmodified-Since", "If-Match", "If-None-Match",
	} {
		if r.Header.Get(header) != "" {
			http.Error(w, fmt.Sprintf("unsupported precondition %s", header), http.StatusBadRequest)
			return nil
		}
	}

	webRoot, err := getWebRoot(r)
	if err != nil {
		return err
	}

	if _, err = AuthorizeUpdateFromArchive(r); err != nil {
		return err
	}

	if checkDryRun(w, r) {
		return nil
	}

	// Providing atomic compare-and-swap operations might be difficult or impossible depending
	// on the backend in use and its configuration, but for applications where a mostly-atomic
	// compare-and-swap operation is good enough (e.g. generating page previews) we don't want
	// to prevent the use of partial updates.
	wantAtomicCAS := r.Header.Get("Atomic")
	hasAtomicCAS := backend.HasAtomicCAS(r.Context())
	switch {
	case wantAtomicCAS == "yes" && hasAtomicCAS || wantAtomicCAS == "no":
		// all good
	case wantAtomicCAS == "yes":
		http.Error(w, "atomic partial updates unsupported", http.StatusPreconditionFailed)
		return nil
	case wantAtomicCAS == "":
		http.Error(w, "must provide \"Atomic: yes|no\" header", http.StatusPreconditionRequired)
		return nil
	default:
		http.Error(w, "malformed Atomic: header", http.StatusBadRequest)
		return nil
	}

	var parents CreateParentsMode
	switch r.Header.Get("Create-Parents") {
	case "", "no":
		parents = RequireParents
	case "yes":
		parents = CreateParents
	default:
		http.Error(w, "malformed Create-Parents: header", http.StatusBadRequest)
		return nil
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(config.Limits.UpdateTimeout))
	defer cancel()

	contentType := getMediaType(r.Header.Get("Content-Type"))
	reader := http.MaxBytesReader(w, r.Body, int64(config.Limits.MaxSiteSize.Bytes()))
	result := PartialUpdateFromArchive(ctx, webRoot, contentType, reader, parents)
	return reportUpdateResult(w, r, result)
}

func reportUpdateResult(w http.ResponseWriter, r *http.Request, result UpdateResult) error {
	var unresolvedRefErr UnresolvedRefError
	if result.outcome == UpdateError && errors.As(result.err, &unresolvedRefErr) {
		offeredContentTypes := []string{"text/plain", "application/vnd.git-pages.unresolved"}
		acceptedContentTypes := ParseAcceptHeader(r.Header.Get("Accept"))
		switch acceptedContentTypes.Negotiate(offeredContentTypes...) {
		default:
			w.Header().Set("Accept", strings.Join(offeredContentTypes, ", "))
			w.WriteHeader(http.StatusNotAcceptable)
			return fmt.Errorf("no supported content types (Accept: %s)", r.Header.Get("Accept"))
		case "application/vnd.git-pages.unresolved":
			w.Header().Set("Content-Type", "application/vnd.git-pages.unresolved")
			w.WriteHeader(http.StatusUnprocessableEntity)
			for _, missingRef := range unresolvedRefErr.missing {
				fmt.Fprintln(w, missingRef)
			}
			return nil
		case "text/plain":
			// handled below
		}
	}

	switch result.outcome {
	case UpdateError:
		if errors.Is(result.err, ErrSiteTooLarge) {
			w.WriteHeader(http.StatusUnprocessableEntity)
		} else if errors.Is(result.err, ErrManifestTooLarge) {
			w.WriteHeader(http.StatusUnprocessableEntity)
		} else if errors.Is(result.err, errArchiveFormat) {
			w.WriteHeader(http.StatusUnsupportedMediaType)
		} else if errors.Is(result.err, ErrArchiveTooLarge) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		} else if errors.Is(result.err, ErrRepositoryTooLarge) {
			w.WriteHeader(http.StatusUnprocessableEntity)
		} else if errors.Is(result.err, ErrMalformedPatch) {
			w.WriteHeader(http.StatusUnprocessableEntity)
		} else if errors.Is(result.err, ErrPreconditionFailed) {
			w.WriteHeader(http.StatusPreconditionFailed)
		} else if errors.Is(result.err, ErrWriteConflict) {
			w.WriteHeader(http.StatusConflict)
		} else if errors.Is(result.err, ErrDomainFrozen) {
			w.WriteHeader(http.StatusForbidden)
		} else if errors.As(result.err, &unresolvedRefErr) {
			w.WriteHeader(http.StatusUnprocessableEntity)
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
	observeSiteUpdate("rest", &result)
	return nil
}

func deletePage(w http.ResponseWriter, r *http.Request) error {
	webRoot, err := getWebRoot(r)
	if err != nil {
		return err
	}

	_, err = AuthorizeUpdateFromRepository(r)
	if err != nil {
		return err
	}

	if checkDryRun(w, r) {
		return nil
	}

	if err = backend.DeleteManifest(r.Context(), webRoot, ModifyManifestOptions{}); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, err)
	} else {
		w.Header().Add("Update-Result", "deleted")
		w.WriteHeader(http.StatusOK)
	}
	return err
}

func postPage(w http.ResponseWriter, r *http.Request) error {
	// Start a timer for the request timeout immediately.
	requestTimeout := 3 * time.Second
	requestTimer := time.NewTimer(requestTimeout)

	webRoot, err := getWebRoot(r)
	if err != nil {
		return err
	}

	auth, err := AuthorizeUpdateFromRepository(r)
	if err != nil {
		return err
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

	if event.Ref != path.Join("refs", "heads", auth.branch) {
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
		observeSiteUpdate("webhook", &result)
	}(context.WithoutCancel(r.Context()))

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
	r = r.WithContext(WithPrincipal(r.Context()))
	if config.Audit.IncludeIPs != "" {
		GetPrincipal(r.Context()).IpAddress = proto.String(r.RemoteAddr)
	}
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
	allowedMethods := []string{"OPTIONS", "HEAD", "GET", "PUT", "PATCH", "DELETE", "POST"}
	if r.Method == "OPTIONS" || !slices.Contains(allowedMethods, r.Method) {
		w.Header().Add("Allow", strings.Join(allowedMethods, ", "))
	}
	err := error(nil)
	switch r.Method {
	// REST API
	case "OPTIONS":
		// no preflight options
	case "HEAD", "GET":
		err = getPage(w, r)
	case "PUT":
		err = putPage(w, r)
	case "PATCH":
		err = patchPage(w, r)
	case "DELETE":
		err = deletePage(w, r)
	// webhook API
	case "POST":
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
