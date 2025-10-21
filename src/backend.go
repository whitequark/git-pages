package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

var errNotFound = errors.New("not found")

func splitBlobName(name string) []string {
	algo, hash, found := strings.Cut(name, "-")
	if found {
		return slices.Concat([]string{algo}, splitBlobName(hash))
	} else {
		return []string{name[0:2], name[2:4], name[4:]}
	}
}

type GetManifestOptions struct {
	BypassCache bool
}

type Backend interface {
	// Retrieve a blob. Returns `reader, size, mtime, err`.
	GetBlob(ctx context.Context, name string) (reader io.ReadSeeker, size uint64, mtime time.Time, err error)

	// Store a blob. If a blob called `name` already exists, this function returns `nil` without
	// regards to the old or new contents. It is expected that blobs are content-addressed, i.e.
	// the `name` contains a cryptographic hash of `data`, but the backend is ignorant of this.
	PutBlob(ctx context.Context, name string, data []byte) error

	// Delete a blob. This is an unconditional operation that can break integrity of manifests.
	DeleteBlob(ctx context.Context, name string) error

	// Retrieve a manifest.
	GetManifest(ctx context.Context, name string, opts GetManifestOptions) (*Manifest, error)

	// Stage a manifest. This operation stores a new version of a manifest, locking any blobs
	// referenced from it in place (for garbage collection purposes) but without any other side
	// effects.
	StageManifest(ctx context.Context, manifest *Manifest) error

	// Commit a manifest. This is an atomic operation; `GetManifest` calls will return either
	// the old version or the new version of the manifest, never anything else.
	CommitManifest(ctx context.Context, name string, manifest *Manifest) error

	// Delete a manifest.
	DeleteManifest(ctx context.Context, name string) error

	// Check whether a domain has any deployments.
	CheckDomain(ctx context.Context, domain string) (found bool, err error)
}

// Retrieve several manifests. This operation succeeds if all requested manifests could be
// retrieved, and fails otherwise. The returned error is the first error that occurs.
func GetManifests(
	backend Backend, ctx context.Context, names []string, opts GetManifestOptions,
) (
	manifests map[string]*Manifest, err error,
) {
	type Result struct {
		name     string
		manifest *Manifest
		err      error
	}

	wg := sync.WaitGroup{}
	ch := make(chan Result, len(names))
	for _, name := range names {
		wg.Go(func() {
			manifest, err := backend.GetManifest(ctx, name, opts)
			ch <- Result{name, manifest, err}
		})
	}
	wg.Wait()
	close(ch)

	manifests = make(map[string]*Manifest)
	for result := range ch {
		if result.err == nil {
			manifests[result.name] = result.manifest
		} else {
			err = result.err
			break
		}
	}
	return
}

var backend Backend

func ConfigureBackend(config *StorageConfig) (err error) {
	switch config.Type {
	case "fs":
		if backend, err = NewFSBackend(&config.FS); err != nil {
			err = fmt.Errorf("fs backend: %w", err)
		}

	case "s3":
		if backend, err = NewS3Backend(context.Background(), &config.S3); err != nil {
			err = fmt.Errorf("s3 backend: %w", err)
		}

	default:
		err = fmt.Errorf("unknown backend: %s", config.Type)
	}
	return
}
