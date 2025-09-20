//go:generate protoc --go_out=. --go_opt=paths=source_relative schema.proto

package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"path"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func IsManifestEmpty(manifest *Manifest) bool {
	if len(manifest.Contents) > 1 {
		return false
	}
	for name, entry := range manifest.Contents {
		if name == "" && entry.GetType() == Type_Directory {
			return true
		}
	}
	panic(fmt.Errorf("malformed manifest %v", manifest))
}

// Returns `true` if `left` and `right` contain the same files with the same types and data.
func CompareManifest(left *Manifest, right *Manifest) bool {
	if len(left.Contents) != len(right.Contents) {
		return false
	}
	for name, leftEntry := range left.Contents {
		rightEntry := right.Contents[name]
		if rightEntry == nil {
			return false
		}
		if leftEntry.GetType() != rightEntry.GetType() {
			return false
		}
		if !bytes.Equal(leftEntry.Data, rightEntry.Data) {
			return false
		}
	}
	return true
}

func EncodeManifest(manifest *Manifest) []byte {
	result, err := proto.MarshalOptions{Deterministic: true}.Marshal(manifest)
	if err != nil {
		panic(err)
	}
	return result
}

func DecodeManifest(data []byte) (*Manifest, error) {
	manifest := Manifest{}
	err := proto.Unmarshal(data, &manifest)
	return &manifest, err
}

func ManifestDebugJSON(manifest *Manifest) string {
	result, err := protojson.MarshalOptions{
		Multiline:         true,
		EmitDefaultValues: true,
	}.Marshal(manifest)
	if err != nil {
		panic(err)
	}
	return string(result)
}

const maxSymlinkLevels int = 128

var errSymlinkLoop = errors.New("symbolic link loop")

func ExpandSymlinks(manifest *Manifest, inPath string) (string, error) {
	var levels int
again:
	for levels = 0; levels < maxSymlinkLevels; levels += 1 {
		parts := strings.Split(inPath, "/")
		for i := 1; i <= len(parts); i++ {
			linkPath := path.Join(parts[:i]...)
			entry := manifest.Contents[linkPath]
			if entry != nil && entry.GetType() == Type_Symlink {
				inPath = path.Join(
					path.Dir(linkPath),
					string(entry.Data),
					path.Join(parts[i:]...),
				)
				continue again
			}
		}
		break
	}
	if levels < maxSymlinkLevels {
		return inPath, nil
	} else {
		return "", errSymlinkLoop
	}
}

// Apply post-processing steps to the manifest.
// At the moment, there isn't a good way to report errors except to log them on the terminal.
// (Perhaps in the future they could be exposed at `.git-pages/status.txt`?)
func PrepareManifest(manifest *Manifest) error {
	// Parse Netlify-style `_redirects`
	if err := ProcessRedirects(manifest); err != nil {
		log.Printf("redirects err: %s\n", err)
	} else if len(manifest.Redirects) > 0 {
		log.Printf("redirects ok: %d rules\n", len(manifest.Redirects))
	}

	return nil
}

const ExternalSizeMin uint32 = 256

// Replaces inline file data over certain size with references to an external content-addressable
// store, without performing any I/O. Returns an updated copy of the manifest.
func ExternalizeFiles(manifest *Manifest) *Manifest {
	newManifest := Manifest{
		RepoUrl:   manifest.RepoUrl,
		Branch:    manifest.Branch,
		Commit:    manifest.Commit,
		Contents:  make(map[string]*Entry),
		Redirects: manifest.Redirects,
	}
	var totalSize uint32
	for name, entry := range manifest.Contents {
		if entry.GetType() == Type_InlineFile && entry.GetSize() > ExternalSizeMin {
			newManifest.Contents[name] = &Entry{
				Type: Type_ExternalFile.Enum(),
				Size: entry.Size,
				Data: fmt.Appendf(nil, "sha256-%x", sha256.Sum256(entry.Data)),
			}
		} else {
			newManifest.Contents[name] = entry
		}
		totalSize += entry.GetSize()
	}
	newManifest.TotalSize = proto.Uint32(totalSize)
	return &newManifest
}

const ManifestSizeMax int = 1048576

var errManifestTooLarge = errors.New("manifest size limit exceeded")

// Uploads inline file data over certain size to the storage backend. Returns a copy of
// the manifest updated to refer to an external content-addressable store.
func StoreManifest(name string, manifest *Manifest) (*Manifest, error) {
	extManifest := ExternalizeFiles(manifest)
	extManifestData := EncodeManifest(extManifest)
	if len(extManifestData) > ManifestSizeMax {
		return nil, fmt.Errorf("%w: %d > %d bytes",
			errManifestTooLarge, extManifestData, ManifestSizeMax)
	}

	if err := backend.StageManifest(extManifest); err != nil {
		return nil, fmt.Errorf("stage manifest: %w", err)
	}

	wg := sync.WaitGroup{}
	ch := make(chan error, len(extManifest.Contents))
	for name, entry := range extManifest.Contents {
		if entry.GetType() == Type_ExternalFile {
			wg.Go(func() {
				err := backend.PutBlob(string(entry.Data), manifest.Contents[name].Data)
				if err != nil {
					ch <- fmt.Errorf("put blob %s: %w", name, err)
				}
			})
		}
	}
	wg.Wait()
	close(ch)
	for err := range ch {
		return nil, err // currently ignores all but 1st error
	}

	if err := backend.CommitManifest(name, extManifest); err != nil {
		return nil, fmt.Errorf("commit manifest: %w", err)
	}

	return extManifest, nil
}
