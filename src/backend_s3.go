package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"path"
	"time"

	"github.com/maypok86/otter/v2"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

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

func makeCacheOptions[K comparable, V any](
	config CacheConfig,
	weigher func(K, V) uint32,
) *otter.Options[K, V] {
	options := &otter.Options[K, V]{}
	if config.MaxSize != 0 {
		options.MaximumWeight = config.MaxSize.Bytes()
		options.Weigher = weigher
	}
	if config.MaxAge != 0 {
		options.ExpiryCalculator = otter.ExpiryWriting[K, V](time.Duration(config.MaxAge) * time.Second)
	}
	return options
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
		log.Printf("s3: create bucket %s\n", config.Backend.S3.Bucket)

		err = client.MakeBucket(ctx, config.Backend.S3.Bucket,
			minio.MakeBucketOptions{Region: config.Backend.S3.Region})
		if err != nil {
			return nil, err
		}
	}

	blobCache, err := otter.New(makeCacheOptions(config.Backend.S3.BlobCache,
		func(key string, value *CachedBlob) uint32 { return uint32(len(value.blob)) }))
	if err != nil {
		return nil, err
	}

	siteCache, err := otter.New(makeCacheOptions(config.Backend.S3.SiteCache,
		func(key string, value *CachedManifest) uint32 { return value.weight }))
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
		// Note that many errors (e.g. NoSuchKey) will be reported only after this point.
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
		if errResp := minio.ToErrorResponse(err); errResp.Code == "NoSuchKey" {
			err = fmt.Errorf("%w: %s", errNotFound, errResp.Key)
		}
		return nil, time.Time{}, err
	} else {
		return bytes.NewReader(cached.blob), cached.mtime, err
	}
}

func (s3 *S3Backend) PutBlob(name string, data []byte) error {
	log.Printf("s3: put blob %s (%d bytes)\n", name, len(data))

	_, err := s3.client.StatObject(s3.ctx, s3.bucket, blobObjectName(name),
		minio.GetObjectOptions{})
	if err != nil {
		if errResp := minio.ToErrorResponse(err); errResp.Code == "NoSuchKey" {
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
			// Note that many errors (e.g. NoSuchKey) will be reported only after this point.
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
			if errResp := minio.ToErrorResponse(err); errResp.Code == "NoSuchKey" {
				err = fmt.Errorf("%w: %s", errNotFound, errResp.Key)
			}
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
	s3.siteCache.Invalidate(name)
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

	err := s3.client.RemoveObject(s3.ctx, s3.bucket, manifestObjectName(name),
		minio.RemoveObjectOptions{})
	s3.siteCache.Invalidate(name)
	return err
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
