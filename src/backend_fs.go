package git_pages

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"iter"
	"os"
	"path/filepath"
	"strings"
)

type FSBackend struct {
	blobRoot     *os.Root
	siteRoot     *os.Root
	auditRoot    *os.Root
	hasAtomicCAS bool
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

func checkAtomicCAS(root *os.Root) bool {
	fileName := ".hasAtomicCAS"
	file, err := root.Create(fileName)
	if err != nil {
		panic(err)
	}
	root.Remove(fileName)
	defer file.Close()

	flockErr := FileLock(file)
	funlockErr := FileUnlock(file)
	return (flockErr == nil && funlockErr == nil)
}

func NewFSBackend(ctx context.Context, config *FSConfig) (*FSBackend, error) {
	blobRoot, err := maybeCreateOpenRoot(config.Root, "blob")
	if err != nil {
		return nil, fmt.Errorf("blob: %w", err)
	}
	siteRoot, err := maybeCreateOpenRoot(config.Root, "site")
	if err != nil {
		return nil, fmt.Errorf("site: %w", err)
	}
	auditRoot, err := maybeCreateOpenRoot(config.Root, "audit")
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	hasAtomicCAS := checkAtomicCAS(siteRoot)
	if hasAtomicCAS {
		logc.Println(ctx, "fs: has atomic CAS")
	} else {
		logc.Println(ctx, "fs: has best-effort CAS")
	}
	return &FSBackend{blobRoot, siteRoot, auditRoot, hasAtomicCAS}, nil
}

func (fs *FSBackend) Backend() Backend {
	return fs
}

func (fs *FSBackend) HasFeature(ctx context.Context, feature BackendFeature) bool {
	switch feature {
	case FeatureCheckDomainMarker:
		return true
	default:
		return false
	}
}

func (fs *FSBackend) EnableFeature(ctx context.Context, feature BackendFeature) error {
	switch feature {
	case FeatureCheckDomainMarker:
		return nil
	default:
		return fmt.Errorf("not implemented")
	}
}

func (fs *FSBackend) GetBlob(
	ctx context.Context, name string,
) (
	reader io.ReadSeeker, metadata BlobMetadata, err error,
) {
	blobPath := filepath.Join(splitBlobName(name)...)
	stat, err := fs.blobRoot.Stat(blobPath)
	if errors.Is(err, os.ErrNotExist) {
		err = fmt.Errorf("%w: %s", ErrObjectNotFound, err.(*os.PathError).Path)
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
	return file, BlobMetadata{name, int64(stat.Size()), stat.ModTime()}, nil
}

func (fs *FSBackend) PutBlob(ctx context.Context, name string, data []byte) error {
	blobPath := filepath.Join(splitBlobName(name)...)
	blobDir := filepath.Dir(blobPath)

	if _, err := fs.blobRoot.Stat(blobPath); err == nil {
		// Blob already exists. While on Linux it would be benign to write and replace a blob
		// that already exists, on Windows this is liable to cause access errors.
		return nil
	}

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

func (fs *FSBackend) EnumerateBlobs(ctx context.Context) iter.Seq2[BlobMetadata, error] {
	return func(yield func(BlobMetadata, error) bool) {
		iofs.WalkDir(fs.blobRoot.FS(), ".",
			func(path string, entry iofs.DirEntry, err error) error {
				var metadata BlobMetadata
				if err != nil {
					// report error
				} else if entry.IsDir() {
					// skip directory
					return nil
				} else if info, err := entry.Info(); err != nil {
					// report error
				} else {
					// report blob
					metadata.Name = joinBlobName(strings.Split(path, "/"))
					metadata.Size = info.Size()
					metadata.LastModified = info.ModTime()
				}
				if !yield(metadata, err) {
					return iofs.SkipAll
				}
				return nil
			})
	}
}

func (fs *FSBackend) ListManifests(ctx context.Context) (manifests []string, err error) {
	err = iofs.WalkDir(fs.siteRoot.FS(), ".",
		func(path string, entry iofs.DirEntry, err error) error {
			if strings.Count(path, "/") > 1 {
				return iofs.SkipDir
			}
			_, project, _ := strings.Cut(path, "/")
			if project == "" || strings.HasPrefix(project, ".") && project != ".index" {
				return nil
			}
			manifests = append(manifests, path)
			return nil
		})
	return
}

func (fs *FSBackend) GetManifest(
	ctx context.Context, name string, opts GetManifestOptions,
) (
	manifest *Manifest, metadata ManifestMetadata, err error,
) {
	stat, err := fs.siteRoot.Stat(name)
	if errors.Is(err, os.ErrNotExist) {
		err = fmt.Errorf("%w: %s", ErrObjectNotFound, err.(*os.PathError).Path)
		return
	} else if err != nil {
		err = fmt.Errorf("stat: %w", err)
		return
	}
	data, err := fs.siteRoot.ReadFile(name)
	if err != nil {
		err = fmt.Errorf("read: %w", err)
		return
	}
	manifest, err = DecodeManifest(data)
	if err != nil {
		return
	}
	return manifest, ManifestMetadata{
		LastModified: stat.ModTime(),
		ETag:         fmt.Sprintf("%x", sha256.Sum256(data)),
	}, nil
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

func domainFrozenMarkerName(domain string) string {
	return filepath.Join(domain, ".frozen")
}

func (fs *FSBackend) checkDomainFrozen(ctx context.Context, domain string) error {
	if _, err := fs.siteRoot.Stat(domainFrozenMarkerName(domain)); err == nil {
		return ErrDomainFrozen
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat: %w", err)
	} else {
		return nil
	}
}

func (fs *FSBackend) HasAtomicCAS(ctx context.Context) bool {
	// On a suitable filesystem, POSIX advisory locks can be used to implement atomic CAS.
	// An implementation consists of two parts:
	//   - Intra-process mutex set (one per manifest), to prevent races between goroutines;
	//   - Inter-process POSIX advisory locks (one per manifest), to prevent races between
	//     different git-pages instances.
	return fs.hasAtomicCAS
}

type manifestLockGuard struct {
	file *os.File
}

func lockManifest(fs *os.Root, name string) (*manifestLockGuard, error) {
	file, err := fs.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return &manifestLockGuard{nil}, nil
	} else if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := FileLock(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("flock(LOCK_EX): %w", err)
	}
	return &manifestLockGuard{file}, nil
}

func (guard *manifestLockGuard) Unlock() {
	if guard.file != nil {
		FileUnlock(guard.file)
		guard.file.Close()
	}
}

func (fs *FSBackend) checkManifestPrecondition(
	ctx context.Context, name string, opts ModifyManifestOptions,
) error {
	if !opts.IfUnmodifiedSince.IsZero() {
		stat, err := fs.siteRoot.Stat(name)
		if err != nil {
			return fmt.Errorf("stat: %w", err)
		}

		if stat.ModTime().Compare(opts.IfUnmodifiedSince) > 0 {
			return fmt.Errorf("%w: If-Unmodified-Since", ErrPreconditionFailed)
		}
	}

	if opts.IfMatch != "" {
		data, err := fs.siteRoot.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		if fmt.Sprintf("%x", sha256.Sum256(data)) != opts.IfMatch {
			return fmt.Errorf("%w: If-Match", ErrPreconditionFailed)
		}
	}

	return nil
}

func (fs *FSBackend) CommitManifest(
	ctx context.Context, name string, manifest *Manifest, opts ModifyManifestOptions,
) error {
	if fs.hasAtomicCAS {
		if guard, err := lockManifest(fs.siteRoot, name); err != nil {
			return err
		} else {
			defer guard.Unlock()
		}
	}

	domain := filepath.Dir(name)
	if err := fs.checkDomainFrozen(ctx, domain); err != nil {
		return err
	}

	if err := fs.checkManifestPrecondition(ctx, name, opts); err != nil {
		return err
	}

	manifestData := EncodeManifest(manifest)
	manifestHashName := stagedManifestName(manifestData)

	if _, err := fs.siteRoot.Stat(manifestHashName); err != nil {
		return fmt.Errorf("manifest not staged")
	}

	if err := fs.siteRoot.MkdirAll(domain, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	if err := fs.siteRoot.Rename(manifestHashName, name); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

func (fs *FSBackend) DeleteManifest(
	ctx context.Context, name string, opts ModifyManifestOptions,
) error {
	if fs.hasAtomicCAS {
		if guard, err := lockManifest(fs.siteRoot, name); err != nil {
			return err
		} else {
			defer guard.Unlock()
		}
	}

	domain := filepath.Dir(name)
	if err := fs.checkDomainFrozen(ctx, domain); err != nil {
		return err
	}

	if err := fs.checkManifestPrecondition(ctx, name, opts); err != nil {
		return err
	}

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

func (fs *FSBackend) CreateDomain(ctx context.Context, domain string) error {
	return nil // no-op
}

func (fs *FSBackend) FreezeDomain(ctx context.Context, domain string, freeze bool) error {
	if freeze {
		return fs.siteRoot.WriteFile(domainFrozenMarkerName(domain), []byte{}, 0o644)
	} else {
		err := fs.siteRoot.Remove(domainFrozenMarkerName(domain))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		} else {
			return err
		}
	}
}

func (fs *FSBackend) AppendAuditLog(ctx context.Context, id AuditID, record *AuditRecord) error {
	if _, err := fs.auditRoot.Stat(id.String()); err == nil {
		panic(fmt.Errorf("audit ID collision: %s", id))
	}

	return fs.auditRoot.WriteFile(id.String(), EncodeAuditRecord(record), 0o644)
}

func (fs *FSBackend) QueryAuditLog(ctx context.Context, id AuditID) (*AuditRecord, error) {
	if data, err := fs.auditRoot.ReadFile(id.String()); err != nil {
		return nil, fmt.Errorf("read: %w", err)
	} else if record, err := DecodeAuditRecord(data); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	} else {
		return record, nil
	}
}

func (fs *FSBackend) SearchAuditLog(
	ctx context.Context, opts SearchAuditLogOptions,
) iter.Seq2[AuditID, error] {
	return func(yield func(AuditID, error) bool) {
		iofs.WalkDir(fs.auditRoot.FS(), ".",
			func(path string, entry iofs.DirEntry, err error) error {
				if path == "." {
					return nil // skip
				}
				var id AuditID
				if err != nil {
					// report error
				} else if id, err = ParseAuditID(path); err != nil {
					// report error
				} else if !opts.Since.IsZero() && id.CompareTime(opts.Since) < 0 {
					return nil // skip
				} else if !opts.Until.IsZero() && id.CompareTime(opts.Until) > 0 {
					return nil // skip
				}
				if !yield(id, err) {
					return iofs.SkipAll // break
				} else {
					return nil // continue
				}
			})
	}
}
