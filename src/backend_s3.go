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

	"github.com/c2h5oh/datasize"
	"github.com/maypok86/otter/v2"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	blobsDedupedCount prometheus.Counter
	blobsDedupedBytes prometheus.Counter

	blobCacheHitsCount      prometheus.Counter
	blobCacheHitsBytes      prometheus.Counter
	blobCacheMissesCount    prometheus.Counter
	blobCacheMissesBytes    prometheus.Counter
	blobCacheEvictionsCount prometheus.Counter
	blobCacheEvictionsBytes prometheus.Counter

	manifestCacheHitsCount      prometheus.Counter
	manifestCacheMissesCount    prometheus.Counter
	manifestCacheEvictionsCount prometheus.Counter
)

func initS3BackendMetrics() {
	blobsDedupedCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blobs_deduped",
		Help: "Count of blobs deduplicated",
	})
	blobsDedupedBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blobs_deduped_bytes",
		Help: "Total size in bytes of blobs deduplicated",
	})

	blobCacheHitsCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blob_cache_hits_count",
		Help: "Count of blobs that were retrieved from the cache",
	})
	blobCacheHitsBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blob_cache_hits_bytes",
		Help: "Total size in bytes of blobs that were retrieved from the cache",
	})
	blobCacheMissesCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blob_cache_misses_count",
		Help: "Count of blobs that were not found in the cache (and were then successfully cached)",
	})
	blobCacheMissesBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blob_cache_misses_bytes",
		Help: "Total size in bytes of blobs that were not found in the cache (and were then successfully cached)",
	})
	blobCacheEvictionsCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blob_cache_evictions_count",
		Help: "Count of blobs evicted from the cache",
	})
	blobCacheEvictionsBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blob_cache_evictions_bytes",
		Help: "Total size in bytes of blobs evicted from the cache",
	})

	manifestCacheHitsCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_manifest_cache_hits_count",
		Help: "Count of manifests that were retrieved from the cache",
	})
	manifestCacheMissesCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_manifest_cache_misses_count",
		Help: "Count of manifests that were not found in the cache (and were then successfully cached)",
	})
	manifestCacheEvictionsCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_manifest_cache_evictions_count",
		Help: "Count of manifests evicted from the cache",
	})
}

// Blobs can be safely cached indefinitely. They only need to be evicted to preserve memory.
type CachedBlob struct {
	blob  []byte
	mtime time.Time
}

func (c *CachedBlob) Weight() uint32 { return uint32(len(c.blob)) }

// Manifests can only be cached for a short time to avoid serving stale content. Browser
// page loads cause a large burst of manifest accesses that are essential for serving
// `304 No Content` responses and these need to be handled very quickly, so both hits and
// misses are cached.
type CachedManifest struct {
	manifest *Manifest
	weight   uint32
	err      error
}

func (c *CachedManifest) Weight() uint32 { return c.weight }

type S3Backend struct {
	client    *minio.Client
	bucket    string
	blobCache *observedCache[string, *CachedBlob]
	siteCache *observedCache[string, *CachedManifest]
}

var _ Backend = (*S3Backend)(nil)

func makeCacheOptions[K comparable, V any](
	config *CacheConfig,
	weigher func(K, V) uint32,
) *otter.Options[K, V] {
	options := &otter.Options[K, V]{}
	if config.MaxSize != 0 {
		options.MaximumWeight = config.MaxSize.Bytes()
		options.Weigher = weigher
	}
	if config.MaxStale != 0 {
		options.RefreshCalculator = otter.RefreshWriting[K, V](time.Duration(config.MaxAge))
	}
	if config.MaxAge != 0 || config.MaxStale != 0 {
		options.ExpiryCalculator = otter.ExpiryWriting[K, V](time.Duration(config.MaxAge + config.MaxStale))
	}
	return options
}

func NewS3Backend(ctx context.Context, config *S3Config) (*S3Backend, error) {
	client, err := minio.New(config.Endpoint, &minio.Options{
		Creds: credentials.NewStaticV4(
			config.AccessKeyID,
			config.SecretAccessKey,
			"",
		),
		Secure: !config.Insecure,
	})
	if err != nil {
		return nil, err
	}

	bucket := config.Bucket
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, err
	} else if !exists {
		log.Printf("s3: create bucket %s\n", bucket)

		err = client.MakeBucket(ctx, bucket,
			minio.MakeBucketOptions{Region: config.Region})
		if err != nil {
			return nil, err
		}
	}

	initS3BackendMetrics()

	blobCacheMetrics := observedCacheMetrics{
		HitNumberCounter:      blobCacheHitsCount,
		HitWeightCounter:      blobCacheHitsBytes,
		MissNumberCounter:     blobCacheMissesCount,
		MissWeightCounter:     blobCacheMissesBytes,
		EvictionNumberCounter: blobCacheEvictionsCount,
		EvictionWeightCounter: blobCacheEvictionsBytes,
	}
	blobCache, err := newObservedCache(makeCacheOptions(&config.BlobCache,
		func(key string, value *CachedBlob) uint32 { return uint32(len(value.blob)) }),
		blobCacheMetrics)
	if err != nil {
		return nil, err
	}

	siteCacheMetrics := observedCacheMetrics{
		HitNumberCounter:      manifestCacheHitsCount,
		MissNumberCounter:     manifestCacheMissesCount,
		EvictionNumberCounter: manifestCacheEvictionsCount,
	}
	siteCache, err := newObservedCache(makeCacheOptions(&config.SiteCache,
		func(key string, value *CachedManifest) uint32 { return value.weight }),
		siteCacheMetrics)
	if err != nil {
		return nil, err
	}

	return &S3Backend{client, bucket, blobCache, siteCache}, nil
}

func (s3 *S3Backend) Backend() Backend {
	return s3
}

func blobObjectName(name string) string {
	return fmt.Sprintf("blob/%s", path.Join(splitBlobName(name)...))
}

func (s3 *S3Backend) GetBlob(
	ctx context.Context,
	name string,
) (
	reader io.ReadSeeker,
	size uint64,
	mtime time.Time,
	err error,
) {
	loader := func(ctx context.Context, name string) (*CachedBlob, error) {
		log.Printf("s3: get blob %s\n", name)

		object, err := s3.client.GetObject(ctx, s3.bucket, blobObjectName(name),
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

	var cached *CachedBlob
	cached, err = s3.blobCache.Get(ctx, name, otter.LoaderFunc[string, *CachedBlob](loader))
	if err != nil {
		if errResp := minio.ToErrorResponse(err); errResp.Code == "NoSuchKey" {
			err = fmt.Errorf("%w: %s", errNotFound, errResp.Key)
		}
	} else {
		reader = bytes.NewReader(cached.blob)
		size = uint64(len(cached.blob))
		mtime = cached.mtime
	}
	return
}

func (s3 *S3Backend) PutBlob(ctx context.Context, name string, data []byte) error {
	log.Printf("s3: put blob %s (%s)\n", name, datasize.ByteSize(len(data)).HumanReadable())

	_, err := s3.client.StatObject(ctx, s3.bucket, blobObjectName(name),
		minio.GetObjectOptions{})
	if err != nil {
		if errResp := minio.ToErrorResponse(err); errResp.Code == "NoSuchKey" {
			_, err := s3.client.PutObject(ctx, s3.bucket, blobObjectName(name),
				bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
			if err != nil {
				return err
			} else {
				ObserveData(ctx, "blob.status", "created")
				log.Printf("s3: put blob %s (created)\n", name)
				return nil
			}
		} else {
			return err
		}
	} else {
		ObserveData(ctx, "blob.status", "exists")
		log.Printf("s3: put blob %s (exists)\n", name)
		blobsDedupedCount.Inc()
		blobsDedupedBytes.Add(float64(len(data)))
		return nil
	}
}

func (s3 *S3Backend) DeleteBlob(ctx context.Context, name string) error {
	log.Printf("s3: delete blob %s\n", name)

	return s3.client.RemoveObject(ctx, s3.bucket, blobObjectName(name),
		minio.RemoveObjectOptions{})
}

func manifestObjectName(name string) string {
	return fmt.Sprintf("site/%s", name)
}

func stagedManifestObjectName(manifestData []byte) string {
	return fmt.Sprintf("dirty/%x", sha256.Sum256(manifestData))
}

func (s3 *S3Backend) GetManifest(ctx context.Context, name string, opts GetManifestOptions) (*Manifest, error) {
	loader := func(ctx context.Context, name string) (*CachedManifest, error) {
		manifest, size, err := func() (*Manifest, uint32, error) {
			log.Printf("s3: get manifest %s\n", name)

			object, err := s3.client.GetObject(ctx, s3.bucket, manifestObjectName(name),
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
				return &CachedManifest{nil, 1, err}, nil
			} else {
				return nil, err
			}
		} else {
			return &CachedManifest{manifest, size, err}, nil
		}
	}

	if opts.BypassCache {
		entry, found := s3.siteCache.Cache.GetEntry(name)
		if found && entry.RefreshableAt().Before(time.Now()) {
			s3.siteCache.Cache.Invalidate(name)
		}
	}

	cached, err := s3.siteCache.Get(ctx, name, otter.LoaderFunc[string, *CachedManifest](loader))
	if err != nil {
		return nil, err
	} else {
		return cached.manifest, cached.err
	}
}

func (s3 *S3Backend) StageManifest(ctx context.Context, manifest *Manifest) error {
	data := EncodeManifest(manifest)
	log.Printf("s3: stage manifest %x\n", sha256.Sum256(data))

	_, err := s3.client.PutObject(ctx, s3.bucket, stagedManifestObjectName(data),
		bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	return err
}

func (s3 *S3Backend) CommitManifest(ctx context.Context, name string, manifest *Manifest) error {
	data := EncodeManifest(manifest)
	log.Printf("s3: commit manifest %x -> %s", sha256.Sum256(data), name)

	// Remove staged object unconditionally (whether commit succeeded or failed), since
	// the upper layer has to retry the complete operation anyway.
	_, putErr := s3.client.PutObject(ctx, s3.bucket, manifestObjectName(name),
		bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	removeErr := s3.client.RemoveObject(ctx, s3.bucket, stagedManifestObjectName(data),
		minio.RemoveObjectOptions{})
	s3.siteCache.Cache.Invalidate(name)
	if putErr != nil {
		return putErr
	} else if removeErr != nil {
		return removeErr
	} else {
		return nil
	}
}

func (s3 *S3Backend) DeleteManifest(ctx context.Context, name string) error {
	log.Printf("s3: delete manifest %s\n", name)

	err := s3.client.RemoveObject(ctx, s3.bucket, manifestObjectName(name),
		minio.RemoveObjectOptions{})
	s3.siteCache.Cache.Invalidate(name)
	return err
}

func (s3 *S3Backend) CheckDomain(ctx context.Context, domain string) (bool, error) {
	log.Printf("s3: check domain %s\n", domain)

	ctx, cancel := context.WithCancel(ctx)
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
