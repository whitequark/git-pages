package git_pages

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type FSBackend struct {
	blobRoot *os.Root
	siteRoot *os.Root
}

var _ Backend = (*FSBackend)(nil)

func maybeCreateOpenRoot(dir string, name string) (*os.Root, error) {
	dirName := filepath.Join(dir, name)

	if err := os.Mkdir(dirName, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("mkdir: %w", err)
	}

	root, err := os.OpenRoot(dirName)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}

	return root, nil
}

func createTempInRoot(root *os.Root, name string, data []byte) (string, error) {
	tempFile, err := os.CreateTemp(root.Name(), name)
	if err != nil {
		return "", fmt.Errorf("mktemp: %w", err)
	}
	_, err = tempFile.Write(data)
	tempFile.Close()
	if err != nil {
		return "", fmt.Errorf("write: %w", err)
	}

	tempPath, err := filepath.Rel(root.Name(), tempFile.Name())
	if err != nil {
		return "", fmt.Errorf("relpath: %w", err)
	}

	return tempPath, nil
}

func NewFSBackend(config *FSConfig) (*FSBackend, error) {
	blobRoot, err := maybeCreateOpenRoot(config.Root, "blob")
	if err != nil {
		return nil, fmt.Errorf("blob: %w", err)
	}
	siteRoot, err := maybeCreateOpenRoot(config.Root, "site")
	if err != nil {
		return nil, fmt.Errorf("site: %w", err)
	}
	return &FSBackend{blobRoot, siteRoot}, nil
}

func (fs *FSBackend) Backend() Backend {
	return fs
}

func (fs *FSBackend) GetBlob(
	ctx context.Context,
	name string,
) (
	reader io.ReadSeeker,
	size uint64,
	mtime time.Time,
	err error,
) {
	blobPath := filepath.Join(splitBlobName(name)...)
	stat, err := fs.blobRoot.Stat(blobPath)
	if errors.Is(err, os.ErrNotExist) {
		err = fmt.Errorf("%w: %s", errNotFound, err.(*os.PathError).Path)
		return
	} else if err != nil {
		err = fmt.Errorf("stat: %w", err)
		return
	}
	file, err := fs.blobRoot.Open(blobPath)
	if err != nil {
		err = fmt.Errorf("open: %w", err)
		return
	}
	return file, uint64(stat.Size()), stat.ModTime(), nil
}

func (fs *FSBackend) PutBlob(ctx context.Context, name string, data []byte) error {
	blobPath := filepath.Join(splitBlobName(name)...)
	blobDir := filepath.Dir(blobPath)

	tempPath, err := createTempInRoot(fs.blobRoot, name, data)
	if err != nil {
		return err
	}

	if err := fs.blobRoot.Chmod(tempPath, 0o444); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

again:
	for {
		if err := fs.blobRoot.MkdirAll(blobDir, 0o755); err != nil {
			if errors.Is(err, os.ErrExist) {
				// Handle the case where two `PutBlob()` calls race creating a common prefix
				// of a blob directory. The `MkdirAll()` call that loses the TOCTTOU condition
				// bails out, so we have to repeat it.
				continue again
			}
			return fmt.Errorf("mkdir: %w", err)
		}
		break
	}

	if err := fs.blobRoot.Rename(tempPath, blobPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

func (fs *FSBackend) DeleteBlob(ctx context.Context, name string) error {
	blobPath := filepath.Join(splitBlobName(name)...)
	return fs.blobRoot.Remove(blobPath)
}

func (fs *FSBackend) GetManifest(ctx context.Context, name string, opts GetManifestOptions) (*Manifest, error) {
	data, err := fs.siteRoot.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", errNotFound, err.(*os.PathError).Path)
	} else if err != nil {
		return nil, err
	}

	return DecodeManifest(data)
}

func stagedManifestName(manifestData []byte) string {
	return fmt.Sprintf(".%x", sha256.Sum256(manifestData))
}

func (fs *FSBackend) StageManifest(ctx context.Context, manifest *Manifest) error {
	manifestData := EncodeManifest(manifest)

	tempPath, err := createTempInRoot(fs.siteRoot, ".manifest", manifestData)
	if err != nil {
		return err
	}

	if err := fs.siteRoot.Rename(tempPath, stagedManifestName(manifestData)); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

func (fs *FSBackend) CommitManifest(ctx context.Context, name string, manifest *Manifest) error {
	manifestData := EncodeManifest(manifest)
	manifestHashName := stagedManifestName(manifestData)

	if _, err := fs.siteRoot.Stat(manifestHashName); err != nil {
		return fmt.Errorf("manifest not staged")
	}

	if err := fs.siteRoot.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	if err := fs.siteRoot.Rename(manifestHashName, name); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

func (fs *FSBackend) DeleteManifest(ctx context.Context, name string) error {
	err := fs.siteRoot.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else {
		return err
	}
}

func (fs *FSBackend) CheckDomain(ctx context.Context, domain string) (bool, error) {
	_, err := fs.siteRoot.Stat(domain)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err == nil {
		return true, nil
	} else {
		return false, err
	}
}
