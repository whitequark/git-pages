package git_pages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

type UpdateOutcome int

const (
	UpdateError UpdateOutcome = iota
	UpdateTimeout
	UpdateCreated
	UpdateReplaced
	UpdateDeleted
	UpdateNoChange
)

type UpdateResult struct {
	outcome  UpdateOutcome
	manifest *Manifest
	err      error
}

func Update(ctx context.Context, webRoot string, manifest *Manifest) UpdateResult {
	var oldManifest, newManifest *Manifest
	var err error

	outcome := UpdateError
	oldManifest, _, _ = backend.GetManifest(ctx, webRoot, GetManifestOptions{})
	if IsManifestEmpty(manifest) {
		newManifest, err = manifest, backend.DeleteManifest(ctx, webRoot)
		if err == nil {
			if oldManifest == nil {
				outcome = UpdateNoChange
			} else {
				outcome = UpdateDeleted
			}
		}
	} else if err = PrepareManifest(ctx, manifest); err == nil {
		newManifest, err = StoreManifest(ctx, webRoot, manifest)
		if err == nil {
			domain, _, _ := strings.Cut(webRoot, "/")
			err = backend.CreateDomain(ctx, domain)
		}
		if err == nil {
			if oldManifest == nil {
				outcome = UpdateCreated
			} else if CompareManifest(oldManifest, newManifest) {
				outcome = UpdateNoChange
			} else {
				outcome = UpdateReplaced
			}
		}
	}

	if err == nil {
		status := ""
		switch outcome {
		case UpdateCreated:
			status = "created"
		case UpdateReplaced:
			status = "replaced"
		case UpdateDeleted:
			status = "deleted"
		case UpdateNoChange:
			status = "unchanged"
		}
		if newManifest.Commit != nil {
			logc.Printf(ctx, "update %s ok: %s %s", webRoot, status, *newManifest.Commit)
		} else {
			logc.Printf(ctx, "update %s ok: %s", webRoot, status)
		}
	} else {
		logc.Printf(ctx, "update %s err: %s", webRoot, err)
	}

	return UpdateResult{outcome, newManifest, err}
}

func UpdateFromRepository(
	ctx context.Context,
	webRoot string,
	repoURL string,
	branch string,
) (result UpdateResult) {
	span, ctx := ObserveFunction(ctx, "UpdateFromRepository", "repo.url", repoURL)
	defer span.Finish()

	logc.Printf(ctx, "update %s: %s %s\n", webRoot, repoURL, branch)

	oldManifest, _, _ := backend.GetManifest(ctx, webRoot, GetManifestOptions{})
	// Ignore errors; worst case we have to re-fetch all of the blobs.

	manifest, err := FetchRepository(ctx, repoURL, branch, oldManifest)
	if errors.Is(err, context.DeadlineExceeded) {
		result = UpdateResult{UpdateTimeout, nil, fmt.Errorf("update timeout")}
	} else if err != nil {
		result = UpdateResult{UpdateError, nil, err}
	} else {
		result = Update(ctx, webRoot, manifest)
	}

	observeUpdateResult(result)
	return result
}

var errArchiveFormat = errors.New("unsupported archive format")

func UpdateFromArchive(
	ctx context.Context,
	webRoot string,
	contentType string,
	reader io.Reader,
) (result UpdateResult) {
	var manifest *Manifest
	var err error

	switch contentType {
	case "application/x-tar":
		logc.Printf(ctx, "update %s: (tar)", webRoot)
		manifest, err = ExtractTar(reader) // yellow?
	case "application/x-tar+gzip":
		logc.Printf(ctx, "update %s: (tar.gz)", webRoot)
		manifest, err = ExtractGzip(reader, ExtractTar) // definitely yellow.
	case "application/x-tar+zstd":
		logc.Printf(ctx, "update %s: (tar.zst)", webRoot)
		manifest, err = ExtractZstd(reader, ExtractTar)
	case "application/zip":
		logc.Printf(ctx, "update %s: (zip)", webRoot)
		manifest, err = ExtractZip(reader)
	default:
		err = errArchiveFormat
	}

	if err != nil {
		logc.Printf(ctx, "update %s err: %s", webRoot, err)
		result = UpdateResult{UpdateError, nil, err}
	} else {
		result = Update(ctx, webRoot, manifest)
	}

	observeUpdateResult(result)
	return
}

func observeUpdateResult(result UpdateResult) {
	if result.err != nil {
		ObserveError(result.err)
	}
}
