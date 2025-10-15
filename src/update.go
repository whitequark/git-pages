package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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
	oldManifest, _ = backend.GetManifest(ctx, webRoot, GetManifestOptions{})
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
			log.Printf("update %s ok: %s %s", webRoot, status, *newManifest.Commit)
		} else {
			log.Printf("update %s ok: %s", webRoot, status)
		}
	} else {
		log.Printf("update %s err: %s", webRoot, err)
	}

	return UpdateResult{outcome, newManifest, err}
}

func UpdateFromRepository(
	ctx context.Context,
	webRoot string,
	repoURL string,
	branch string,
) UpdateResult {
	span, ctx := ObserveFunction(ctx, "UpdateFromRepository", "repo.url", repoURL)
	defer span.Finish()

	log.Printf("update %s: %s %s\n", webRoot, repoURL, branch)

	manifest, err := FetchRepository(ctx, repoURL, branch)
	if errors.Is(err, context.DeadlineExceeded) {
		return UpdateResult{UpdateTimeout, nil, fmt.Errorf("update timeout")}
	} else if err != nil {
		return UpdateResult{UpdateError, nil, err}
	} else {
		return Update(ctx, webRoot, manifest)
	}
}

var errArchiveFormat = errors.New("unsupported archive format")

func UpdateFromArchive(
	ctx context.Context,
	webRoot string,
	contentType string,
	reader io.Reader,
) UpdateResult {
	var manifest *Manifest
	var err error

	switch contentType {
	case "application/x-tar":
		log.Printf("update %s: (tar)", webRoot)
		manifest, err = ExtractTar(reader) // yellow?
	case "application/x-tar+gzip":
		log.Printf("update %s: (tar.gz)", webRoot)
		manifest, err = ExtractTarGzip(reader) // definitely yellow.
	case "application/x-tar+zstd":
		log.Printf("update %s: (tar.zst)", webRoot)
		manifest, err = ExtractTarZstd(reader)
	case "application/zip":
		log.Printf("update %s: (zip)", webRoot)
		manifest, err = ExtractZip(reader)
	default:
		err = errArchiveFormat
	}

	if err != nil {
		log.Printf("update %s err: %s", webRoot, err)
		return UpdateResult{UpdateError, nil, err}
	} else {
		return Update(ctx, webRoot, manifest)
	}
}
