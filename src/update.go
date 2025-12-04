package git_pages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/protobuf/proto"
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

func Update(
	ctx context.Context, webRoot string, oldManifest, newManifest *Manifest,
	opts ModifyManifestOptions,
) UpdateResult {
	var err error
	var storedManifest *Manifest

	outcome := UpdateError
	if IsManifestEmpty(newManifest) {
		storedManifest, err = newManifest, backend.DeleteManifest(ctx, webRoot, opts)
		if err == nil {
			if oldManifest == nil {
				outcome = UpdateNoChange
			} else {
				outcome = UpdateDeleted
			}
		}
	} else if err = PrepareManifest(ctx, newManifest); err == nil {
		storedManifest, err = StoreManifest(ctx, webRoot, newManifest, opts)
		if err == nil {
			domain, _, _ := strings.Cut(webRoot, "/")
			err = backend.CreateDomain(ctx, domain)
		}
		if err == nil {
			if oldManifest == nil {
				outcome = UpdateCreated
			} else if CompareManifest(oldManifest, storedManifest) {
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
		if storedManifest.Commit != nil {
			logc.Printf(ctx, "update %s ok: %s %s", webRoot, *storedManifest.Commit, status)
		} else {
			logc.Printf(ctx, "update %s ok: %s", webRoot, status)
		}
	} else {
		logc.Printf(ctx, "update %s err: %s", webRoot, err)
	}

	return UpdateResult{outcome, storedManifest, err}
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

	// Ignore errors; worst case we have to re-fetch all of the blobs.
	oldManifest, _, _ := backend.GetManifest(ctx, webRoot, GetManifestOptions{})

	newManifest, err := FetchRepository(ctx, repoURL, branch, oldManifest)
	if errors.Is(err, context.DeadlineExceeded) {
		result = UpdateResult{UpdateTimeout, nil, fmt.Errorf("update timeout")}
	} else if err != nil {
		result = UpdateResult{UpdateError, nil, err}
	} else {
		result = Update(ctx, webRoot, oldManifest, newManifest, ModifyManifestOptions{})
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
	var err error

	// Ignore errors; here the old manifest is used only to determine the update outcome.
	oldManifest, _, _ := backend.GetManifest(ctx, webRoot, GetManifestOptions{})

	var newManifest *Manifest
	switch contentType {
	case "application/x-tar":
		logc.Printf(ctx, "update %s: (tar)", webRoot)
		newManifest, err = ExtractTar(reader) // yellow?
	case "application/x-tar+gzip":
		logc.Printf(ctx, "update %s: (tar.gz)", webRoot)
		newManifest, err = ExtractGzip(reader, ExtractTar) // definitely yellow.
	case "application/x-tar+zstd":
		logc.Printf(ctx, "update %s: (tar.zst)", webRoot)
		newManifest, err = ExtractZstd(reader, ExtractTar)
	case "application/zip":
		logc.Printf(ctx, "update %s: (zip)", webRoot)
		newManifest, err = ExtractZip(reader)
	default:
		err = errArchiveFormat
	}

	if err != nil {
		logc.Printf(ctx, "update %s err: %s", webRoot, err)
		result = UpdateResult{UpdateError, nil, err}
	} else {
		result = Update(ctx, webRoot, oldManifest, newManifest, ModifyManifestOptions{})
	}

	observeUpdateResult(result)
	return
}

func PartialUpdateFromArchive(
	ctx context.Context,
	webRoot string,
	contentType string,
	reader io.Reader,
) (result UpdateResult) {
	var err error

	// Here the old manifest is used both as a substrate to which a patch is applied, as well
	// as a "load linked" operation for a future "store conditional" update which, taken together,
	// create an atomic compare-and-swap operation.
	oldManifest, oldMetadata, err := backend.GetManifest(ctx, webRoot,
		GetManifestOptions{BypassCache: true})
	if err != nil {
		logc.Printf(ctx, "patch %s err: %s", webRoot, err)
		return UpdateResult{UpdateError, nil, err}
	}

	applyTarPatch := func(reader io.Reader) (*Manifest, error) {
		// Clone the manifest before starting to mutate it. `GetManifest` may return cached
		// `*Manifest` objects, which should never be mutated.
		newManifest := &Manifest{}
		proto.Merge(newManifest, oldManifest)
		if err := ApplyTarPatch(newManifest, reader); err != nil {
			return nil, err
		} else {
			return newManifest, nil
		}
	}

	var newManifest *Manifest
	switch contentType {
	case "application/x-tar":
		logc.Printf(ctx, "patch %s: (tar)", webRoot)
		newManifest, err = applyTarPatch(reader)
	case "application/x-tar+gzip":
		logc.Printf(ctx, "patch %s: (tar.gz)", webRoot)
		newManifest, err = ExtractGzip(reader, applyTarPatch)
	case "application/x-tar+zstd":
		logc.Printf(ctx, "patch %s: (tar.zst)", webRoot)
		newManifest, err = ExtractZstd(reader, applyTarPatch)
	default:
		err = errArchiveFormat
	}

	if err != nil {
		logc.Printf(ctx, "patch %s err: %s", webRoot, err)
		result = UpdateResult{UpdateError, nil, err}
	} else {
		result = Update(ctx, webRoot, oldManifest, newManifest,
			ModifyManifestOptions{
				IfUnmodifiedSince: oldMetadata.LastModified,
				IfMatch:           oldMetadata.ETag,
			})
		// The `If-Unmodified-Since` precondition is internally generated here, which means its
		// failure shouldn't be surfaced as-is in the HTTP response. If we also accepted options
		// from the client, then that precondition failure should surface in the response.
		if errors.Is(result.err, ErrPreconditionFailed) {
			result.err = ErrWriteConflict
		}
	}

	observeUpdateResult(result)
	return
}

func observeUpdateResult(result UpdateResult) {
	if result.err != nil {
		ObserveError(result.err)
	}
}
