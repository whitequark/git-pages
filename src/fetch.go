package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v6/osfs"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage/memory"
)

func splitHash(hash plumbing.Hash) string {
	head := hash.String()
	return filepath.Join(head[:2], head[2:])
}

func fetch(
	dataDir string,
	webroot string,
	url string,
	branch plumbing.ReferenceName,
) (*plumbing.Hash, error) {
	storer := memory.NewStorage()

	repo, err := git.Clone(storer, nil, &git.CloneOptions{
		URL:           url,
		ReferenceName: branch,
		SingleBranch:  true,
		Depth:         1,
		Tags:          git.NoTags,
	})
	if err != nil {
		return nil, fmt.Errorf("git clone: %s", err)
	}

	ref, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("git head: %s", err)
	}
	head := ref.Hash()

	destDir := filepath.Join(dataDir, "tree", splitHash(head))
	if _, err := os.Stat(destDir); errors.Is(err, os.ErrNotExist) {
		// check out to a temporary directory to avoid TOCTTOU race on destDir
		tempDir, err := os.MkdirTemp(dataDir, ".tree")
		if err != nil {
			return nil, fmt.Errorf("mkdir temp: %s", err)
		}
		defer os.RemoveAll(tempDir)

		repo, err = git.Open(storer, osfs.New(tempDir))
		if err != nil {
			return nil, fmt.Errorf("git open: %s", err)
		}

		worktree, err := repo.Worktree()
		if err != nil {
			return nil, fmt.Errorf("git worktree: %s", err)
		}

		if err := worktree.Checkout(&git.CheckoutOptions{
			Hash: head,
		}); err != nil {
			return nil, fmt.Errorf("git checkout: %s", err)
		}

		if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir parent dest: %s", err)
		}

		// commit atomically; assume another fetch has won the race if directory exists
		if err := os.Rename(tempDir, destDir); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("rename dest: %s", err)
		}
	}

	webLink := filepath.Join(dataDir, "www", webroot)
	destDirRel, _ := filepath.Rel(filepath.Dir(webLink), destDir)

	tempLink := filepath.Join(dataDir,
		fmt.Sprintf(".link.%s.%s", strings.ReplaceAll(webroot, "/", ".."), head.String()))
	if err := os.Symlink(destDirRel, tempLink); err != nil {
		return nil, fmt.Errorf("symlink temp: %s", err)
	}
	defer os.Remove(tempLink)

	if err := os.MkdirAll(filepath.Dir(webLink), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir parent web: %s", err)
	}

	// commit atomically; assume another fetch has won the race if symlink exists
	// FIXME: might not have the same target
	if err := os.Rename(tempLink, webLink); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("rename web: %s", err)
	}

	return &head, nil
}

func Fetch(
	dataDir string,
	webroot string,
	url string,
	branch plumbing.ReferenceName,
) error {
	log.Println("fetch:", webroot, url, branch)

	head, err := fetch(dataDir, webroot, url, branch)
	if err != nil {
		log.Println("fetch err:", fmt.Errorf("%s: %s", webroot, err))
		return err
	}

	log.Println("fetch ok:", webroot, head)
	return nil
}
