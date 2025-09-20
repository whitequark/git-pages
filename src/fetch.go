package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/go-git/go-billy/v6/osfs"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"google.golang.org/protobuf/proto"
)

const largeObjectThreshold int64 = 1048576

func FetchRepository(ctx context.Context, repoURL string, branch string) (*Manifest, error) {
	baseDir, err := os.MkdirTemp("", "fetchRepo")
	if err != nil {
		return nil, fmt.Errorf("mkdtemp: %w", err)
	}
	defer os.RemoveAll(baseDir)

	fs := osfs.New(baseDir, osfs.WithBoundOS())
	cache := cache.NewObjectLRUDefault()
	storer := filesystem.NewStorageWithOptions(fs, cache, filesystem.Options{
		ExclusiveAccess:      true,
		LargeObjectThreshold: largeObjectThreshold,
	})
	repo, err := git.CloneContext(ctx, storer, nil, &git.CloneOptions{
		Bare:          true,
		URL:           repoURL,
		ReferenceName: plumbing.ReferenceName(branch),
		SingleBranch:  true,
		Depth:         1,
		Tags:          git.NoTags,
	})
	if err != nil {
		return nil, fmt.Errorf("git clone: %w", err)
	}

	ref, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("git head: %w", err)
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("git commit: %w", err)
	}

	tree, err := repo.TreeObject(commit.TreeHash)
	if err != nil {
		return nil, fmt.Errorf("git tree: %w", err)
	}

	walker := object.NewTreeWalker(tree, true, make(map[plumbing.Hash]bool))
	defer walker.Close()

	manifest := Manifest{
		RepoUrl: proto.String(repoURL),
		Branch:  proto.String(branch),
		Commit:  proto.String(ref.Hash().String()),
		Contents: map[string]*Entry{
			"": {Type: Type_Directory.Enum()},
		},
	}
	for {
		name, entry, err := walker.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("git walker: %w", err)
		} else {
			manifestEntry := Entry{}
			if entry.Mode.IsFile() {
				blob, err := repo.BlobObject(entry.Hash)
				if err != nil {
					return nil, fmt.Errorf("git blob %s: %w", name, err)
				}

				reader, err := blob.Reader()
				if err != nil {
					return nil, fmt.Errorf("git blob open: %w", err)
				}
				defer reader.Close()

				data, err := io.ReadAll(reader)
				if err != nil {
					return nil, fmt.Errorf("git blob read: %w", err)
				}

				if entry.Mode == filemode.Symlink {
					manifestEntry.Type = Type_Symlink.Enum()
				} else {
					manifestEntry.Type = Type_InlineFile.Enum()
				}
				manifestEntry.Size = proto.Uint32(uint32(blob.Size))
				manifestEntry.Data = data
			} else if entry.Mode == filemode.Dir {
				manifestEntry.Type = Type_Directory.Enum()
			} else {
				manifestEntry.Type = Type_Invalid.Enum()
			}
			manifest.Contents[name] = &manifestEntry
		}
	}
	return &manifest, nil
}
