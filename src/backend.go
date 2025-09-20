// Abstract interface for storage backends; filesystem backend.

package main

import (
	"errors"
	"io"
	"slices"
	"strings"
	"time"
)

var errNotFound = errors.New("not found")

type Backend interface {
	// Retrieve a blob. Returns `reader, mtime, err`.
	GetBlob(name string) (io.ReadSeeker, time.Time, error)

	// Store a blob. If a blob called `name` already exists, this function returns `nil` without
	// regards to the old or new contents. It is expected that blobs are content-addressed, i.e.
	// the `name` contains a cryptographic hash of `data`, but the backend is ignorant of this.
	PutBlob(name string, data []byte) error

	// Delete a blob. This is an unconditional operation that can break integrity of manifests.
	DeleteBlob(name string) error

	// Retrieve a manifest.
	GetManifest(name string) (*Manifest, error)

	// Stage a manifest. This operation stores a new version of a manifest, locking any blobs
	// referenced from it in place (for garbage collection purposes) but without any other side
	// effects.
	StageManifest(manifest *Manifest) error

	// Commit a manifest. This is an atomic operation; `GetManifest` calls will return either
	// the old version or the new version of the manifest, never anything else.
	CommitManifest(name string, manifest *Manifest) error

	// Delete a manifest.
	DeleteManifest(name string) error

	// Check whether a domain has any deployments.
	CheckDomain(domain string) (bool, error)
}

func splitBlobName(name string) []string {
	algo, hash, found := strings.Cut(name, "-")
	if found {
		return slices.Concat([]string{algo}, splitBlobName(hash))
	} else {
		return []string{name[0:2], name[2:4], name[4:]}
	}
}
