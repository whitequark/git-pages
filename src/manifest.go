package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
)

// Returns `true` if `left` and `right` contain the same files with the same types and data.
func CompareManifest(left *Manifest, right *Manifest) bool {
	if len(left.Tree) != len(right.Tree) {
		return false
	}
	for name, leftEntry := range left.Tree {
		rightEntry := right.Tree[name]
		if rightEntry == nil {
			return false
		}
		if leftEntry.Type != rightEntry.Type {
			return false
		}
		if bytes.Compare(leftEntry.Data, rightEntry.Data) != 0 {
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

const maxSymlinkLevels int = 128

var symlinkLoop = errors.New("symbolic link loop")

func ExpandSymlinks(manifest *Manifest, inPath string) (string, error) {
	var levels int
again:
	for levels = 0; levels < maxSymlinkLevels; levels += 1 {
		parts := strings.Split(inPath, "/")
		for i := 1; i <= len(parts); i++ {
			linkPath := path.Join(parts[:i]...)
			entry := manifest.Tree[linkPath]
			if entry != nil && entry.Type == Type_Symlink {
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
		return "", symlinkLoop
	}
}

const ExternalSizeMin int64 = 256

func ExternalizeFiles(manifest *Manifest) *Manifest {
	newManifest := Manifest{
		RepoURL: manifest.RepoURL,
		Branch:  manifest.Branch,
		Commit:  manifest.Commit,
		Tree:    make(map[string]*Entry),
	}
	for name, entry := range manifest.Tree {
		if entry.Type == Type_InlineFile && entry.Size > ExternalSizeMin {
			newManifest.Tree[name] = &Entry{
				Type: Type_ExternalFile,
				Size: entry.Size,
				Data: fmt.Appendf(nil, "sha256-%x", sha256.Sum256(entry.Data)),
			}
		} else {
			newManifest.Tree[name] = entry
		}
	}
	return &newManifest
}

const ManifestSizeMax int = 1048576

// Accepts a manifest with inline files, returns a manifest with external files after writing
// file contents and the manifest itself to the storage.
func StoreManifest(name string, manifest *Manifest) (*Manifest, error) {
	extManifest := ExternalizeFiles(manifest)
	extManifestData := EncodeManifest(extManifest)
	if len(extManifestData) > ManifestSizeMax {
		return nil, fmt.Errorf("manifest too big: %d > %d bytes", extManifestData, ManifestSizeMax)
	}

	if err := backend.StageManifest(extManifest); err != nil {
		return nil, fmt.Errorf("stage: %w", err)
	}

	wg := sync.WaitGroup{}
	ch := make(chan error, len(extManifest.Tree))
	for name, entry := range extManifest.Tree {
		if entry.Type == Type_ExternalFile {
			wg.Go(func() {
				err := backend.PutBlob(string(entry.Data), manifest.Tree[name].Data)
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
		return nil, fmt.Errorf("commit: %w", err)
	}

	return extManifest, nil
}
