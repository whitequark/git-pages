package git_pages

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
	"time"
)

var ErrObjectNotFound = errors.New("not found")
var ErrPreconditionFailed = errors.New("precondition failed")
var ErrWriteConflict = errors.New("write conflict")
var ErrDomainFrozen = errors.New("domain administratively frozen")

func splitBlobName(name string) []string {
	if algo, hash, found := strings.Cut(name, "-"); found {
		return []string{algo, hash[0:2], hash[2:4], hash[4:]}
	} else {
		panic("malformed blob name")
	}
}

func joinBlobName(parts []string) string {
	return fmt.Sprintf("%s-%s", parts[0], strings.Join(parts[1:], ""))
}

type BackendFeature string

const (
	FeatureCheckDomainMarker BackendFeature = "check-domain-marker"
)

type BlobMetadata struct {
	Name         string
	Size         int64
	LastModified time.Time
}

type GetManifestOptions struct {
	// If true and the manifest is past the cache `MaxAge`, `GetManifest` blocks and returns
	// a fresh object instead of revalidating in background and returning a stale object.
	BypassCache bool
}

type ManifestMetadata struct {
	LastModified time.Time
	ETag         string
}

type ModifyManifestOptions struct {
	// If non-zero, the request will only succeed if the manifest hasn't been changed since
	// the given time. Whether this is racy or not is can be determined via `HasAtomicCAS()`.
	IfUnmodifiedSince time.Time
	// If non-empty, the request will only succeed if the manifest hasn't changed from
	// the state corresponding to the ETag. Whether this is racy or not is can be determined
	// via `HasAtomicCAS()`.
	IfMatch string
}

type SearchAuditLogOptions struct {
	// Inclusive lower bound on returned audit records, per their Snowflake ID (which may differ
	// slightly from the embedded timestamp). If zero, audit records are returned since beginning
	// of time.
	Since time.Time
	// Inclusive upper bound on returned audit records, per their Snowflake ID (which may differ
	// slightly from the embedded timestamp). If zero, audit records are returned until the end
	// of time.
	Until time.Time
}

type SearchAuditLogResult struct {
	ID  AuditID
	Err error
}

type Backend interface {
	// Returns true if the feature has been enabled for this store, false otherwise.
	HasFeature(ctx context.Context, feature BackendFeature) bool

	// Enables the feature for this store.
	EnableFeature(ctx context.Context, feature BackendFeature) error

	// Retrieve a blob. Returns `reader, size, mtime, err`.
	GetBlob(ctx context.Context, name string) (
		reader io.ReadSeeker, metadata BlobMetadata, err error,
	)

	// Store a blob. If a blob called `name` already exists, this function returns `nil` without
	// regards to the old or new contents. It is expected that blobs are content-addressed, i.e.
	// the `name` contains a cryptographic hash of `data`, but the backend is ignorant of this.
	PutBlob(ctx context.Context, name string, data []byte) error

	// Delete a blob. This is an unconditional operation that can break integrity of manifests.
	DeleteBlob(ctx context.Context, name string) error

	// Iterate through all blobs. Whether blobs that are newly added during iteration will appear
	// in the results is unspecified.
	EnumerateBlobs(ctx context.Context) iter.Seq2[BlobMetadata, error]

	// Retrieve a manifest.
	GetManifest(ctx context.Context, name string, opts GetManifestOptions) (
		manifest *Manifest, metadata ManifestMetadata, err error,
	)

	// Stage a manifest. This operation stores a new version of a manifest, locking any blobs
	// referenced from it in place (for garbage collection purposes) but without any other side
	// effects.
	StageManifest(ctx context.Context, manifest *Manifest) error

	// Whether a compare-and-swap operation on a manifest is truly race-free, or only best-effort
	// atomic with a small but non-zero window where two requests may race where the one committing
	// first will have its update lost. (Plain swap operations are always guaranteed to be atomic.)
	HasAtomicCAS(ctx context.Context) bool

	// Commit a manifest. This is an atomic operation; `GetManifest` calls will return either
	// the old version or the new version of the manifest, never anything else.
	CommitManifest(ctx context.Context, name string, manifest *Manifest, opts ModifyManifestOptions) error

	// Delete a manifest.
	DeleteManifest(ctx context.Context, name string, opts ModifyManifestOptions) error

	// List all manifests.
	ListManifests(ctx context.Context) (manifests []string, err error)

	// Check whether a domain has any deployments.
	CheckDomain(ctx context.Context, domain string) (found bool, err error)

	// Create a domain. This allows us to start serving content for the domain.
	CreateDomain(ctx context.Context, domain string) error

	// Freeze or thaw a domain. This allows a site to be administratively locked, e.g. if it
	// is discovered serving abusive content.
	FreezeDomain(ctx context.Context, domain string, freeze bool) error

	// Append a record to the audit log.
	AppendAuditLog(ctx context.Context, id AuditID, record *AuditRecord) error

	// Retrieve a single record from the audit log.
	QueryAuditLog(ctx context.Context, id AuditID) (record *AuditRecord, err error)

	// Retrieve records from the audit log by time range.
	SearchAuditLog(ctx context.Context, opts SearchAuditLogOptions) iter.Seq2[AuditID, error]
}

func CreateBackend(ctx context.Context, config *StorageConfig) (backend Backend, err error) {
	switch config.Type {
	case "fs":
		if backend, err = NewFSBackend(ctx, &config.FS); err != nil {
			err = fmt.Errorf("fs backend: %w", err)
		}
	case "s3":
		if backend, err = NewS3Backend(ctx, &config.S3); err != nil {
			err = fmt.Errorf("s3 backend: %w", err)
		}
	default:
		err = fmt.Errorf("unknown backend: %s", config.Type)
	}
	backend = NewAuditedBackend(backend)
	return
}
