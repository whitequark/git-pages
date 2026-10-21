package git_pages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"codeberg.org/git-pages/git-pages/src/names"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const BlobReferencePrefix = "/git/blobs/"

type UnresolvedRefError struct {
	missing []string
}

func (err UnresolvedRefError) Error() string {
	return fmt.Sprintf("%d unresolved blob references", len(err.missing))
}

type UpdateOptions struct {
	expiresAt time.Time
}

func (opts *UpdateOptions) Apply(manifest *Manifest) {
	if !opts.expiresAt.IsZero() {
		manifest.ExpiresAt = timestamppb.New(opts.expiresAt)
	}
}

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

var errExpireExistingSite = fmt.Errorf("cannot expire an existing site")

func Update(
	ctx context.Context, site names.Site, oldManifest, newManifest *Manifest,
	opts ModifyManifestOptions,
) UpdateResult {
	var err error
	var storedManifest *Manifest

	outcome := UpdateError
	if oldManifest != nil && oldManifest.ExpiresAt == nil && newManifest.ExpiresAt != nil {
		err = errExpireExistingSite
	} else if IsManifestEmpty(newManifest) {
		storedManifest, err = newManifest, backend.DeleteManifest(ctx, site, opts)
		if err == nil {
			if oldManifest == nil {
				outcome = UpdateNoChange
			} else {
				outcome = UpdateDeleted
			}
		}
	} else if err = PrepareManifest(ctx, site, newManifest); err == nil {
		storedManifest, err = StoreManifest(ctx, site, newManifest, opts)
		if err == nil {
			err = backend.CreateDomain(ctx, site.Domain)
			existenceCache.AddSite(ctx, site)
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
		if newManifest.ExpiresAt != nil {
			logc.Printf(ctx, "expire %s: at %s", site, newManifest.ExpiresAt.AsTime())
		}
		if storedManifest.Commit != nil {
			logc.Printf(ctx, "update %s ok: %s %s", site, *storedManifest.Commit, status)
		} else {
			logc.Printf(ctx, "update %s ok: %s", site, status)
		}
	} else {
		logc.Printf(ctx, "update %s err: %s", site, err)
	}

	return UpdateResult{outcome, storedManifest, err}
}

func UpdateFromRepository(
	ctx context.Context,
	site names.Site,
	repoURL string,
	branch string,
	opts UpdateOptions,
) (result UpdateResult) {
	span, ctx := ObserveFunction(ctx, "UpdateFromRepository", "repo.url", repoURL)
	defer span.Finish()
	defer observeUpdateResult(result)

	logc.Printf(ctx, "update %s: %s %s\n", site, repoURL, branch)

	oldManifest, _, err := backend.GetManifest(ctx, site, GetManifestOptions{})
	if err != nil && !errors.Is(err, ErrObjectNotFound) {
		logc.Printf(ctx, "update %s err: %s", site, err)
		result = UpdateResult{UpdateError, nil, err}
		return
	}

	newManifest, err := FetchRepository(ctx, repoURL, branch, oldManifest)
	if errors.Is(err, context.DeadlineExceeded) {
		result = UpdateResult{UpdateTimeout, nil, fmt.Errorf("update timeout")}
	} else if err != nil {
		result = UpdateResult{UpdateError, nil, err}
	} else {
		opts.Apply(newManifest)
		result = Update(ctx, site, oldManifest, newManifest, ModifyManifestOptions{})
	}

	return
}

var errArchiveFormat = errors.New("unsupported archive format")

func UpdateFromArchive(
	ctx context.Context,
	site names.Site,
	repoURL string,
	contentType string,
	reader io.Reader,
	opts UpdateOptions,
) (result UpdateResult) {
	span, ctx := ObserveFunction(ctx, "UpdateFromArchive",
		"repo.url", repoURL, "archive.type", contentType)
	defer span.Finish()
	defer observeUpdateResult(result)

	oldManifest, _, err := backend.GetManifest(ctx, site, GetManifestOptions{})
	if err != nil && !errors.Is(err, ErrObjectNotFound) {
		logc.Printf(ctx, "update %s err: %s", site, err)
		result = UpdateResult{UpdateError, nil, err}
		return
	}

	extractTar := func(ctx context.Context, reader io.Reader) (*Manifest, error) {
		return ExtractTar(ctx, reader, oldManifest)
	}

	var newManifest *Manifest
	switch contentType {
	case "application/x-tar":
		logc.Printf(ctx, "update %s: (tar)", site)
		newManifest, err = extractTar(ctx, reader) // yellow?
	case "application/x-tar+gzip":
		logc.Printf(ctx, "update %s: (tar.gz)", site)
		newManifest, err = ExtractGzip(ctx, reader, extractTar) // definitely yellow.
	case "application/x-tar+zstd":
		logc.Printf(ctx, "update %s: (tar.zst)", site)
		newManifest, err = ExtractZstd(ctx, reader, extractTar)
	case "application/zip":
		logc.Printf(ctx, "update %s: (zip)", site)
		newManifest, err = ExtractZip(ctx, reader, oldManifest)
	default:
		err = errArchiveFormat
	}

	if err != nil {
		logc.Printf(ctx, "update %s err: %s", site, err)
		result = UpdateResult{UpdateError, nil, err}
	} else {
		if repoURL != "" {
			newManifest.RepoUrl = &repoURL
		}
		opts.Apply(newManifest)
		result = Update(ctx, site, oldManifest, newManifest, ModifyManifestOptions{})
	}
	return
}

func PartialUpdateFromArchive(
	ctx context.Context,
	site names.Site,
	contentType string,
	reader io.Reader,
	parents CreateParentsMode,
	opts UpdateOptions,
) (result UpdateResult) {
	span, ctx := ObserveFunction(ctx, "PartialUpdateFromArchive", "archive.type", contentType)
	defer span.Finish()
	defer observeUpdateResult(result)

	// Here the old manifest is used both as a substrate to which a patch is applied, as well
	// as a "load linked" operation for a future "store conditional" update which, taken together,
	// create an atomic compare-and-swap operation.
	oldManifest, oldMetadata, err := backend.GetManifest(ctx, site,
		GetManifestOptions{BypassCache: true})
	if err != nil {
		logc.Printf(ctx, "patch %s err: %s", site, err)
		result = UpdateResult{UpdateError, nil, err}
		return
	}

	applyTarPatch := func(ctx context.Context, reader io.Reader) (*Manifest, error) {
		// Clone the manifest before starting to mutate it. `GetManifest` may return cached
		// `*Manifest` objects, which should never be mutated.
		newManifest := &Manifest{}
		proto.Merge(newManifest, oldManifest)
		newManifest.RepoUrl = nil
		newManifest.Branch = nil
		newManifest.Commit = nil
		if err := ApplyTarPatch(newManifest, reader, parents); err != nil {
			return nil, err
		} else {
			return newManifest, nil
		}
	}

	var newManifest *Manifest
	switch contentType {
	case "application/x-tar":
		logc.Printf(ctx, "patch %s: (tar)", site)
		newManifest, err = applyTarPatch(ctx, reader)
	case "application/x-tar+gzip":
		logc.Printf(ctx, "patch %s: (tar.gz)", site)
		newManifest, err = ExtractGzip(ctx, reader, applyTarPatch)
	case "application/x-tar+zstd":
		logc.Printf(ctx, "patch %s: (tar.zst)", site)
		newManifest, err = ExtractZstd(ctx, reader, applyTarPatch)
	default:
		err = errArchiveFormat
	}

	if err != nil {
		logc.Printf(ctx, "patch %s err: %s", site, err)
		result = UpdateResult{UpdateError, nil, err}
	} else {
		opts.Apply(newManifest)
		result = Update(ctx, site, oldManifest, newManifest,
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

	return
}

func observeUpdateResult(result UpdateResult) {
	var unresolvedRefErr UnresolvedRefError
	if errors.As(result.err, &unresolvedRefErr) {
		// This error is an expected outcome of an incremental update's probe phase.
	} else if errors.Is(result.err, ErrWriteConflict) {
		// This error is an expected outcome of an incremental update losing a race.
	} else if result.err != nil {
		ObserveError(result.err)
	}
}
