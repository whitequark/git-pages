package main

import (
	"context"
	"fmt"
	"io"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/memory"
)

func FetchRepository(ctx context.Context, repoURL string, branch string) (*Manifest, error) {
	storer := memory.NewStorage()

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
		RepoURL: repoURL,
		Branch:  branch,
		Commit:  ref.Hash().String(),
		Tree:    make(map[string]*Entry),
	}
	manifest.Tree[""] = &Entry{Type: Type_Directory, Size: 0, Data: []byte{}}
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

				data, err := io.ReadAll(reader)
				if err != nil {
					return nil, fmt.Errorf("git blob read: %w", err)
				}

				if entry.Mode == filemode.Symlink {
					manifestEntry.Type = Type_Symlink
				} else {
					manifestEntry.Type = Type_InlineFile
				}
				manifestEntry.Size = blob.Size
				manifestEntry.Data = data
			} else if entry.Mode == filemode.Dir {
				manifestEntry.Type = Type_Directory
			} else {
				manifestEntry.Type = Type_Invalid
			}
			manifest.Tree[name] = &manifestEntry
		}
	}
	return &manifest, nil
}
