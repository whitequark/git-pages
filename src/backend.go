// Abstract interface for storage backends; filesystem backend.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/maypok86/otter/v2"
)

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

type FSBackend struct {
	blobRoot *os.Root
	siteRoot *os.Root
}

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

func NewFSBackend(dir string) (*FSBackend, error) {
	blobRoot, err := maybeCreateOpenRoot(dir, "blob")
	if err != nil {
		return nil, fmt.Errorf("blob: %w", err)
	}
	siteRoot, err := maybeCreateOpenRoot(dir, "site")
	if err != nil {
		return nil, fmt.Errorf("site: %w", err)
	}
	return &FSBackend{blobRoot, siteRoot}, nil
}

func (fs *FSBackend) Backend() Backend {
	return fs
}

func (fs *FSBackend) GetBlob(name string) (io.ReadSeeker, time.Time, error) {
	blobPath := filepath.Join(splitBlobName(name)...)
	stat, err := fs.blobRoot.Stat(blobPath)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("stat: %w", err)
	}
	file, err := fs.blobRoot.Open(blobPath)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("open: %w", err)
	}
	return file, stat.ModTime(), nil
}

func (fs *FSBackend) PutBlob(name string, data []byte) error {
	blobPath := filepath.Join(splitBlobName(name)...)
	blobDir := filepath.Dir(blobPath)

	tempPath, err := createTempInRoot(fs.blobRoot, name, data)
	if err != nil {
		return err
	}

	if err := fs.blobRoot.Chmod(tempPath, 0o444); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	if err := fs.blobRoot.MkdirAll(blobDir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	if err := fs.blobRoot.Rename(tempPath, blobPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

func (fs *FSBackend) DeleteBlob(name string) error {
	blobPath := filepath.Join(splitBlobName(name)...)
	return fs.blobRoot.Remove(blobPath)
}

func (fs *FSBackend) GetManifest(name string) (*Manifest, error) {
	data, err := fs.siteRoot.ReadFile(name)
	if err != nil {
		return nil, err
	}

	return DecodeManifest(data)
}

func stagedManifestName(manifestData []byte) string {
	return fmt.Sprintf(".%x", sha256.Sum256(manifestData))
}

func (fs *FSBackend) StageManifest(manifest *Manifest) error {
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

func (fs *FSBackend) CommitManifest(name string, manifest *Manifest) error {
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

func (fs *FSBackend) DeleteManifest(name string) error {
	return fs.siteRoot.Remove(name)
}

func (fs *FSBackend) CheckDomain(domain string) (bool, error) {
	_, err := fs.siteRoot.Stat(domain)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err == nil {
		return true, nil
	} else {
		return false, err
	}
}

// Blobs can be safely cached indefinitely. They only need to be evicted to preserve memory.
type CachedBlob struct {
	blob  []byte
	mtime time.Time
}

// Manifests can only be cached for a short time to avoid serving stale content. Browser
// page loads cause a large burst of manifest accesses that are essential for serving
// `304 No Content` responses and these need to be handled very quickly, so both hits and
// misses are cached.
type CachedManifest struct {
	manifest *Manifest
	weight   uint32
	err      error
}

type S3Backend struct {
	ctx       context.Context
	client    *minio.Client
	bucket    string
	blobCache *otter.Cache[string, *CachedBlob]
	siteCache *otter.Cache[string, *CachedManifest]
}

func defaultCacheConfig[K comparable, V any](
	config CacheConfig,
	maxAge time.Duration,
	maxSize uint64,
	weigher func(K, V) uint32,
) (*otter.Options[K, V], error) {
	var err error
	if config.MaxAge != "" {
		maxAge, err = time.ParseDuration(config.MaxAge)
		if err != nil {
			return nil, fmt.Errorf("max-age: %w", err)
		}
	}
	if config.MaxSize != 0 {
		maxSize = config.MaxSize
	}

	options := &otter.Options[K, V]{}
	if maxSize != 0 {
		options.MaximumWeight = maxSize
		options.Weigher = weigher
	}
	if maxAge != 0 {
		options.ExpiryCalculator = otter.ExpiryWriting[K, V](maxAge)
	}
	return options, nil
}

func NewS3Backend(
	endpoint string,
	insecure bool,
	accessKeyID string,
	secretAccessKey string,
	region string,
	bucket string,
) (*S3Backend, error) {
	ctx := context.Background()

	client, err := minio.New(config.Backend.S3.Endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(
			config.Backend.S3.AccessKeyID,
			config.Backend.S3.SecretAccessKey,
			"",
		),
		Secure: !config.Backend.S3.Insecure,
	})
	if err != nil {
		return nil, err
	}

	exists, err := client.BucketExists(ctx, config.Backend.S3.Bucket)
	if err != nil {
		return nil, err
	} else if !exists {
		err = client.MakeBucket(ctx, config.Backend.S3.Bucket,
			minio.MakeBucketOptions{Region: config.Backend.S3.Region})
		if err != nil {
			return nil, err
		}
	}

	blobCacheOptions, err := defaultCacheConfig[string, *CachedBlob](
		config.Backend.S3.BlobCache,
		0, 256*1048576,
		func(key string, value *CachedBlob) uint32 { return uint32(len(value.blob)) })
	if err != nil {
		return nil, err
	}

	blobCache, err := otter.New(blobCacheOptions)
	if err != nil {
		return nil, err
	}

	siteCacheOptions, err := defaultCacheConfig[string, *CachedManifest](
		config.Backend.S3.SiteCache,
		60*time.Second, 16*1048576,
		func(key string, value *CachedManifest) uint32 { return value.weight })
	if err != nil {
		return nil, err
	}

	siteCache, err := otter.New(siteCacheOptions)
	if err != nil {
		return nil, err
	}

	return &S3Backend{ctx, client, bucket, blobCache, siteCache}, nil
}

func (s3 *S3Backend) Backend() Backend {
	return s3
}

func blobObjectName(name string) string {
	return fmt.Sprintf("blob/%s", path.Join(splitBlobName(name)...))
}

func (s3 *S3Backend) GetBlob(name string) (io.ReadSeeker, time.Time, error) {
	loader := func(ctx context.Context, name string) (*CachedBlob, error) {
		log.Printf("s3: get blob %s\n", name)

		object, err := s3.client.GetObject(s3.ctx, s3.bucket, blobObjectName(name),
			minio.GetObjectOptions{})
		if err != nil {
			return nil, err
		}
		defer object.Close()

		stat, err := object.Stat()
		if err != nil {
			return nil, err
		}

		data, err := io.ReadAll(object)
		if err != nil {
			return nil, err
		}

		return &CachedBlob{data, stat.LastModified}, nil
	}

	cached, err := s3.blobCache.Get(s3.ctx, name, otter.LoaderFunc[string, *CachedBlob](loader))
	if err != nil {
		return nil, time.Time{}, err
	}

	return bytes.NewReader(cached.blob), cached.mtime, err
}

func (s3 *S3Backend) PutBlob(name string, data []byte) error {
	log.Printf("s3: put blob %s (%d bytes)\n", name, len(data))

	_, err := s3.client.StatObject(s3.ctx, s3.bucket, blobObjectName(name),
		minio.GetObjectOptions{})
	if err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" {
			_, err := s3.client.PutObject(s3.ctx, s3.bucket, blobObjectName(name),
				bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
			if err != nil {
				return err
			} else {
				log.Printf("s3: put blob %s (created)\n", name)
				return nil
			}
		} else {
			return err
		}
	} else {
		log.Printf("s3: put blob %s (exists)\n", name)
		return nil
	}
}

func (s3 *S3Backend) DeleteBlob(name string) error {
	log.Printf("s3: delete blob %s\n", name)

	return s3.client.RemoveObject(s3.ctx, s3.bucket, blobObjectName(name),
		minio.RemoveObjectOptions{})
}

func manifestObjectName(name string) string {
	return fmt.Sprintf("site/%s", name)
}

func stagedManifestObjectName(manifestData []byte) string {
	return fmt.Sprintf("dirty/%x", sha256.Sum256(manifestData))
}

func (s3 *S3Backend) GetManifest(name string) (*Manifest, error) {
	loader := func(ctx context.Context, name string) (*CachedManifest, error) {
		manifest, size, err := func() (*Manifest, uint32, error) {
			log.Printf("s3: get manifest %s\n", name)

			object, err := s3.client.GetObject(s3.ctx, s3.bucket, manifestObjectName(name),
				minio.GetObjectOptions{})
			if err != nil {
				return nil, 0, err
			}
			defer object.Close()

			data, err := io.ReadAll(object)
			if err != nil {
				return nil, 0, err
			}

			manifest, err := DecodeManifest(data)
			if err != nil {
				return nil, 0, err
			}

			return manifest, uint32(len(data)), nil
		}()

		if err != nil {
			return &CachedManifest{nil, 1, err}, nil
		} else {
			return &CachedManifest{manifest, size, err}, nil
		}
	}

	cached, err := s3.siteCache.Get(s3.ctx, name, otter.LoaderFunc[string, *CachedManifest](loader))
	if err != nil {
		return nil, err
	} else {
		return cached.manifest, cached.err
	}
}

func (s3 *S3Backend) StageManifest(manifest *Manifest) error {
	data := EncodeManifest(manifest)
	log.Printf("s3: stage manifest %x\n", sha256.Sum256(data))

	_, err := s3.client.PutObject(s3.ctx, s3.bucket, stagedManifestObjectName(data),
		bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	return err
}

func (s3 *S3Backend) CommitManifest(name string, manifest *Manifest) error {
	data := EncodeManifest(manifest)
	log.Printf("s3: commit manifest %x -> %s", sha256.Sum256(data), name)

	// Remove staged object unconditionally (whether commit succeeded or failed), since
	// the upper layer has to retry the complete operation anyway.
	_, putErr := s3.client.PutObject(s3.ctx, s3.bucket, manifestObjectName(name),
		bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	removeErr := s3.client.RemoveObject(s3.ctx, s3.bucket, stagedManifestObjectName(data),
		minio.RemoveObjectOptions{})
	if putErr != nil {
		return putErr
	} else if removeErr != nil {
		return removeErr
	} else {
		return nil
	}
}

func (s3 *S3Backend) DeleteManifest(name string) error {
	log.Printf("s3: delete manifest %s\n", name)

	return s3.client.RemoveObject(s3.ctx, s3.bucket, manifestObjectName(name),
		minio.RemoveObjectOptions{})
}

func (s3 *S3Backend) CheckDomain(domain string) (bool, error) {
	log.Printf("s3: check domain %s\n", domain)

	ctx, cancel := context.WithCancel(s3.ctx)
	defer cancel()

	for object := range s3.client.ListObjectsIter(ctx, s3.bucket, minio.ListObjectsOptions{
		Prefix: manifestObjectName(fmt.Sprintf("%s/", domain)),
	}) {
		if object.Err != nil {
			return false, object.Err
		}
		return true, nil
	}
	return false, nil
}
