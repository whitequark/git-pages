package git_pages

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"iter"
	"net/http"
	"path"
	"strings"
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

	s3GetObjectDurationSeconds *prometheus.HistogramVec
	s3GetObjectResponseCount   *prometheus.CounterVec
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

	s3GetObjectDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "git_pages_s3_get_object_duration_seconds",
		Help:    "Time to read a whole object from S3",
		Buckets: []float64{.01, .025, .05, .1, .25, .5, .75, 1, 1.25, 1.5, 1.75, 2, 2.5, 5, 10},

		NativeHistogramBucketFactor:     1.1,
		NativeHistogramMaxBucketNumber:  100,
		NativeHistogramMinResetDuration: 10 * time.Minute,
	}, []string{"kind"})
	s3GetObjectResponseCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "git_pages_s3_get_object_responses_count",
		Help: "Count of s3:GetObject responses",
	}, []string{"kind", "code"})
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
	metadata ManifestMetadata
	err      error
}

func (c *CachedManifest) Weight() uint32 { return c.weight }

type S3Backend struct {
	client       *minio.Client
	bucket       string
	blobCache    *observedCache[string, *CachedBlob]
	siteCache    *observedCache[string, *CachedManifest]
	featureCache *otter.Cache[BackendFeature, bool]
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
		options.RefreshCalculator = otter.RefreshWriting[K, V](
			time.Duration(config.MaxAge))
	}
	if config.MaxAge != 0 || config.MaxStale != 0 {
		options.ExpiryCalculator = otter.ExpiryWriting[K, V](
			time.Duration(config.MaxAge + config.MaxStale))
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
		logc.Printf(ctx, "s3: create bucket %s\n", bucket)

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

	featureCache, err := otter.New(&otter.Options[BackendFeature, bool]{
		RefreshCalculator: otter.RefreshWriting[BackendFeature, bool](10 * time.Minute),
	})
	if err != nil {
		return nil, err
	}

	return &S3Backend{client, bucket, blobCache, siteCache, featureCache}, nil
}

func (s3 *S3Backend) Backend() Backend {
	return s3
}

func blobObjectName(name string) string {
	return fmt.Sprintf("blob/%s", path.Join(splitBlobName(name)...))
}

func storeFeatureObjectName(feature BackendFeature) string {
	return fmt.Sprintf("meta/feature/%s", feature)
}

func (s3 *S3Backend) HasFeature(ctx context.Context, feature BackendFeature) bool {
	loader := func(ctx context.Context, feature BackendFeature) (bool, error) {
		_, err := s3.client.StatObject(ctx, s3.bucket, storeFeatureObjectName(feature),
			minio.StatObjectOptions{})
		if err != nil {
			if errResp := minio.ToErrorResponse(err); errResp.Code == "NoSuchKey" {
				logc.Printf(ctx, "s3 feature %q: disabled", feature)
				return false, nil
			} else {
				return false, err
			}
		}
		logc.Printf(ctx, "s3 feature %q: enabled", feature)
		return true, nil
	}

	isOn, err := s3.featureCache.Get(ctx, feature, otter.LoaderFunc[BackendFeature, bool](loader))
	if err != nil {
		err = fmt.Errorf("getting s3 backend feature %q: %w", feature, err)
		ObserveError(err)
		logc.Println(ctx, err)
		return false
	}
	return isOn
}

func (s3 *S3Backend) EnableFeature(ctx context.Context, feature BackendFeature) error {
	_, err := s3.client.PutObject(ctx, s3.bucket, storeFeatureObjectName(feature),
		&bytes.Reader{}, 0, minio.PutObjectOptions{})
	return err
}

func (s3 *S3Backend) GetBlob(
	ctx context.Context, name string,
) (
	reader io.ReadSeeker, metadata BlobMetadata, err error,
) {
	loader := func(ctx context.Context, name string) (*CachedBlob, error) {
		logc.Printf(ctx, "s3: get blob %s\n", name)

		startTime := time.Now()

		object, err := s3.client.GetObject(ctx, s3.bucket, blobObjectName(name),
			minio.GetObjectOptions{})
		// Note that many errors (e.g. NoSuchKey) will be reported only after this point.
		if err != nil {
			return nil, err
		}
		defer object.Close()

		data, err := io.ReadAll(object)
		if err != nil {
			return nil, err
		}

		stat, err := object.Stat()
		if err != nil {
			return nil, err
		}

		s3GetObjectDurationSeconds.
			With(prometheus.Labels{"kind": "blob"}).
			Observe(time.Since(startTime).Seconds())

		return &CachedBlob{data, stat.LastModified}, nil
	}

	observer := func(ctx context.Context, name string) (*CachedBlob, error) {
		cached, err := loader(ctx, name)
		var code = "OK"
		if resp, ok := err.(minio.ErrorResponse); ok {
			code = resp.Code
		}
		s3GetObjectResponseCount.With(prometheus.Labels{"kind": "blob", "code": code}).Inc()
		return cached, err
	}

	var cached *CachedBlob
	cached, err = s3.blobCache.Get(ctx, name, otter.LoaderFunc[string, *CachedBlob](observer))
	if err != nil {
		if errResp := minio.ToErrorResponse(err); errResp.Code == "NoSuchKey" {
			err = fmt.Errorf("%w: %s", ErrObjectNotFound, errResp.Key)
		}
	} else {
		reader = bytes.NewReader(cached.blob)
		metadata.Name = name
		metadata.Size = int64(len(cached.blob))
		metadata.LastModified = cached.mtime
	}
	return
}

func (s3 *S3Backend) PutBlob(ctx context.Context, name string, data []byte) error {
	logc.Printf(ctx, "s3: put blob %s (%s)\n", name, datasize.ByteSize(len(data)).HumanReadable())

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
				logc.Printf(ctx, "s3: put blob %s (created)\n", name)
				return nil
			}
		} else {
			return err
		}
	} else {
		ObserveData(ctx, "blob.status", "exists")
		logc.Printf(ctx, "s3: put blob %s (exists)\n", name)
		blobsDedupedCount.Inc()
		blobsDedupedBytes.Add(float64(len(data)))
		return nil
	}
}

func (s3 *S3Backend) DeleteBlob(ctx context.Context, name string) error {
	logc.Printf(ctx, "s3: delete blob %s\n", name)

	return s3.client.RemoveObject(ctx, s3.bucket, blobObjectName(name),
		minio.RemoveObjectOptions{})
}

func (s3 *S3Backend) EnumerateBlobs(ctx context.Context) iter.Seq2[BlobMetadata, error] {
	return func(yield func(BlobMetadata, error) bool) {
		logc.Print(ctx, "s3: enumerate blobs")

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		prefix := "blob/"
		for object := range s3.client.ListObjectsIter(ctx, s3.bucket, minio.ListObjectsOptions{
			Prefix:    prefix,
			Recursive: true,
		}) {
			var metadata BlobMetadata
			var err error
			if err = object.Err; err == nil {
				key := strings.TrimPrefix(object.Key, prefix)
				if strings.HasSuffix(key, "/") {
					continue // directory; skip
				} else {
					metadata.Name = joinBlobName(strings.Split(key, "/"))
					metadata.Size = object.Size
					metadata.LastModified = object.LastModified
				}
			}
			if !yield(metadata, err) {
				break
			}
		}
	}
}

func manifestObjectName(name string) string {
	return fmt.Sprintf("site/%s", name)
}

func stagedManifestObjectName(manifestData []byte) string {
	return fmt.Sprintf("dirty/%x", sha256.Sum256(manifestData))
}

type s3ManifestLoader struct {
	s3 *S3Backend
}

func (l s3ManifestLoader) Load(
	ctx context.Context, key string,
) (
	*CachedManifest, error,
) {
	return l.load(ctx, key, nil)
}

func (l s3ManifestLoader) Reload(
	ctx context.Context, key string, oldValue *CachedManifest,
) (
	*CachedManifest, error,
) {
	return l.load(ctx, key, oldValue)
}

func (l s3ManifestLoader) load(
	ctx context.Context, name string, oldManifest *CachedManifest,
) (
	*CachedManifest, error,
) {
	logc.Printf(ctx, "s3: get manifest %s\n", name)

	loader := func() (*CachedManifest, error) {
		opts := minio.GetObjectOptions{}
		if oldManifest != nil && oldManifest.metadata.ETag != "" {
			opts.SetMatchETagExcept(oldManifest.metadata.ETag)
		}
		object, err := l.s3.client.GetObject(ctx, l.s3.bucket, manifestObjectName(name), opts)
		// Note that many errors (e.g. NoSuchKey) will be reported only after this point.
		if err != nil {
			return nil, err
		}
		defer object.Close()

		data, err := io.ReadAll(object)
		if err != nil {
			return nil, err
		}

		stat, err := object.Stat()
		if err != nil {
			return nil, err
		}

		manifest, err := DecodeManifest(data)
		if err != nil {
			return nil, err
		}

		metadata := ManifestMetadata{
			LastModified: stat.LastModified,
			ETag:         stat.ETag,
		}
		return &CachedManifest{manifest, uint32(len(data)), metadata, nil}, nil
	}

	observer := func() (*CachedManifest, error) {
		cached, err := loader()
		var code = "OK"
		if resp, ok := err.(minio.ErrorResponse); ok {
			code = resp.Code
		}
		s3GetObjectResponseCount.With(prometheus.Labels{"kind": "manifest", "code": code}).Inc()
		return cached, err
	}

	startTime := time.Now()
	cached, err := observer()
	s3GetObjectDurationSeconds.
		With(prometheus.Labels{"kind": "manifest"}).
		Observe(time.Since(startTime).Seconds())

	if err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "NoSuchKey" {
			err = fmt.Errorf("%w: %s", ErrObjectNotFound, errResp.Key)
			return &CachedManifest{nil, 1, ManifestMetadata{}, err}, nil
		} else if errResp.StatusCode == http.StatusNotModified && oldManifest != nil {
			return oldManifest, nil
		} else {
			return nil, err
		}
	} else {
		return cached, nil
	}
}

func (s3 *S3Backend) GetManifest(
	ctx context.Context, name string, opts GetManifestOptions,
) (
	manifest *Manifest, metadata ManifestMetadata, err error,
) {
	if opts.BypassCache {
		entry, found := s3.siteCache.Cache.GetEntry(name)
		if found && entry.RefreshableAt().Before(time.Now()) {
			s3.siteCache.Cache.Invalidate(name)
		}
	}

	var cached *CachedManifest
	cached, err = s3.siteCache.Get(ctx, name, s3ManifestLoader{s3})
	if err != nil {
		return
	} else {
		// This could be `manifest, mtime, nil` or `nil, time.Time{}, ErrObjectNotFound`.
		manifest, metadata, err = cached.manifest, cached.metadata, cached.err
		return
	}
}

func (s3 *S3Backend) StageManifest(ctx context.Context, manifest *Manifest) error {
	data := EncodeManifest(manifest)
	logc.Printf(ctx, "s3: stage manifest %x\n", sha256.Sum256(data))

	_, err := s3.client.PutObject(ctx, s3.bucket, stagedManifestObjectName(data),
		bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	return err
}

func domainFrozenObjectName(domain string) string {
	return manifestObjectName(fmt.Sprintf("%s/.frozen", domain))
}

func (s3 *S3Backend) checkDomainFrozen(ctx context.Context, domain string) error {
	_, err := s3.client.StatObject(ctx, s3.bucket, domainFrozenObjectName(domain),
		minio.GetObjectOptions{})
	if err == nil {
		return ErrDomainFrozen
	} else if errResp := minio.ToErrorResponse(err); errResp.Code == "NoSuchKey" {
		return nil
	} else {
		return err
	}
}

func (s3 *S3Backend) HasAtomicCAS(ctx context.Context) bool {
	// Support for `If-Unmodified-Since:` or `If-Match:` for PutObject requests is very spotty:
	//   - AWS supports only `If-Match:`:
	//     https://docs.aws.amazon.com/AmazonS3/latest/API/API_PutObject.html
	//   - Minio supports `If-Match:`:
	//     https://blog.min.io/leading-the-way-minios-conditional-write-feature-for-modern-data-workloads/
	//   - Tigris supports `If-Unmodified-Since:` and `If-Match:`, but only with `X-Tigris-Consistent: true`;
	//     https://www.tigrisdata.com/docs/objects/conditionals/
	//     Note that the `X-Tigris-Consistent: true` header must be present on *every* transaction
	//     touching the object, not just on the CAS transactions.
	//   - Wasabi does not support either one and docs seem to suggest that the headers are ignored;
	//   - Garage does not support either one and source code suggests the headers are ignored.
	// It seems that the only safe option is to not claim support for atomic CAS, and only do
	// best-effort CAS implementation using HeadObject and PutObject/DeleteObject.
	return false
}

func (s3 *S3Backend) checkManifestPrecondition(
	ctx context.Context, name string, opts ModifyManifestOptions,
) error {
	if opts.IfUnmodifiedSince.IsZero() && opts.IfMatch == "" {
		return nil
	}

	stat, err := s3.client.StatObject(ctx, s3.bucket, manifestObjectName(name),
		minio.GetObjectOptions{})
	if err != nil {
		return err
	}

	if !opts.IfUnmodifiedSince.IsZero() && stat.LastModified.Compare(opts.IfUnmodifiedSince) > 0 {
		return fmt.Errorf("%w: If-Unmodified-Since", ErrPreconditionFailed)
	}
	if opts.IfMatch != "" && stat.ETag != opts.IfMatch {
		return fmt.Errorf("%w: If-Match", ErrPreconditionFailed)
	}

	return nil
}

func (s3 *S3Backend) CommitManifest(
	ctx context.Context, name string, manifest *Manifest, opts ModifyManifestOptions,
) error {
	data := EncodeManifest(manifest)
	logc.Printf(ctx, "s3: commit manifest %x -> %s", sha256.Sum256(data), name)

	_, domain, _ := strings.Cut(name, "/")
	if err := s3.checkDomainFrozen(ctx, domain); err != nil {
		return err
	}

	if err := s3.checkManifestPrecondition(ctx, name, opts); err != nil {
		return err
	}

	// Remove staged object unconditionally (whether commit succeeded or failed), since
	// the upper layer has to retry the complete operation anyway.
	putOptions := minio.PutObjectOptions{}
	putOptions.Header().Add("X-Tigris-Consistent", "true")
	if opts.IfMatch != "" {
		// Not guaranteed to do anything (see `HasAtomicCAS`), but let's try anyway;
		// this is a "belt and suspenders" approach, together with `checkManifestPrecondition`.
		// It does reliably work on MinIO at least.
		putOptions.SetMatchETag(opts.IfMatch)
	}
	_, putErr := s3.client.PutObject(ctx, s3.bucket, manifestObjectName(name),
		bytes.NewReader(data), int64(len(data)), putOptions)
	removeErr := s3.client.RemoveObject(ctx, s3.bucket, stagedManifestObjectName(data),
		minio.RemoveObjectOptions{})
	s3.siteCache.Cache.Invalidate(name)
	if putErr != nil {
		if errResp := minio.ToErrorResponse(putErr); errResp.Code == "PreconditionFailed" {
			return ErrPreconditionFailed
		} else {
			return putErr
		}
	} else if removeErr != nil {
		return removeErr
	} else {
		return nil
	}
}

func (s3 *S3Backend) DeleteManifest(
	ctx context.Context, name string, opts ModifyManifestOptions,
) error {
	logc.Printf(ctx, "s3: delete manifest %s\n", name)

	_, domain, _ := strings.Cut(name, "/")
	if err := s3.checkDomainFrozen(ctx, domain); err != nil {
		return err
	}

	if err := s3.checkManifestPrecondition(ctx, name, opts); err != nil {
		return err
	}

	err := s3.client.RemoveObject(ctx, s3.bucket, manifestObjectName(name),
		minio.RemoveObjectOptions{})
	s3.siteCache.Cache.Invalidate(name)
	return err
}

func (s3 *S3Backend) EnumerateManifests(ctx context.Context) iter.Seq2[ManifestMetadata, error] {
	return func(yield func(ManifestMetadata, error) bool) {
		logc.Print(ctx, "s3: enumerate manifests")

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		prefix := "site/"
		for object := range s3.client.ListObjectsIter(ctx, s3.bucket, minio.ListObjectsOptions{
			Prefix:    prefix,
			Recursive: true,
		}) {
			var metadata ManifestMetadata
			var err error
			if err = object.Err; err == nil {
				key := strings.TrimPrefix(object.Key, prefix)
				_, project, _ := strings.Cut(key, "/")
				if strings.HasSuffix(key, "/") {
					continue // directory; skip
				} else if project == "" || strings.HasPrefix(project, ".") && project != ".index" {
					continue // internal; skip
				} else {
					metadata.Name = key
					metadata.Size = object.Size
					metadata.LastModified = object.LastModified
					metadata.ETag = object.ETag
				}
			}
			if !yield(metadata, err) {
				break
			}
		}
	}
}

func domainCheckObjectName(domain string) string {
	return manifestObjectName(fmt.Sprintf("%s/.exists", domain))
}

func (s3 *S3Backend) CheckDomain(ctx context.Context, domain string) (exists bool, err error) {
	logc.Printf(ctx, "s3: check domain %s\n", domain)

	_, err = s3.client.StatObject(ctx, s3.bucket, domainCheckObjectName(domain),
		minio.StatObjectOptions{})
	if err != nil {
		if errResp := minio.ToErrorResponse(err); errResp.Code == "NoSuchKey" {
			exists, err = false, nil
		}
	} else {
		exists = true
	}

	if !exists && !s3.HasFeature(ctx, FeatureCheckDomainMarker) {
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

	return
}

func (s3 *S3Backend) CreateDomain(ctx context.Context, domain string) error {
	logc.Printf(ctx, "s3: create domain %s\n", domain)

	_, err := s3.client.PutObject(ctx, s3.bucket, domainCheckObjectName(domain),
		&bytes.Reader{}, 0, minio.PutObjectOptions{})
	return err
}

func (s3 *S3Backend) FreezeDomain(ctx context.Context, domain string, freeze bool) error {
	if freeze {
		logc.Printf(ctx, "s3: freeze domain %s\n", domain)

		_, err := s3.client.PutObject(ctx, s3.bucket, domainFrozenObjectName(domain),
			&bytes.Reader{}, 0, minio.PutObjectOptions{})
		return err
	} else {
		logc.Printf(ctx, "s3: thaw domain %s\n", domain)

		err := s3.client.RemoveObject(ctx, s3.bucket, domainFrozenObjectName(domain),
			minio.RemoveObjectOptions{})
		if errResp := minio.ToErrorResponse(err); errResp.Code == "NoSuchKey" {
			return nil
		} else {
			return err
		}
	}
}

func auditObjectName(id AuditID) string {
	return fmt.Sprintf("audit/%s", id)
}

func (s3 *S3Backend) AppendAuditLog(ctx context.Context, id AuditID, record *AuditRecord) error {
	logc.Printf(ctx, "s3: append audit %s\n", id)

	name := auditObjectName(id)
	data := EncodeAuditRecord(record)

	options := minio.PutObjectOptions{}
	options.SetMatchETagExcept("*") // may or may not be supported
	_, err := s3.client.PutObject(ctx, s3.bucket, name,
		bytes.NewReader(data), int64(len(data)), options)
	if errResp := minio.ToErrorResponse(err); errResp.StatusCode == 412 {
		panic(fmt.Errorf("audit ID collision: %s", name))
	}
	return err
}

func (s3 *S3Backend) QueryAuditLog(ctx context.Context, id AuditID) (*AuditRecord, error) {
	logc.Printf(ctx, "s3: read audit %s\n", id)

	object, err := s3.client.GetObject(ctx, s3.bucket, auditObjectName(id),
		minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer object.Close()

	data, err := io.ReadAll(object)
	if err != nil {
		return nil, err
	}

	return DecodeAuditRecord(data)
}

func (s3 *S3Backend) SearchAuditLog(
	ctx context.Context, opts SearchAuditLogOptions,
) iter.Seq2[AuditID, error] {
	return func(yield func(AuditID, error) bool) {
		logc.Printf(ctx, "s3: query audit\n")

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		prefix := "audit/"
		for object := range s3.client.ListObjectsIter(ctx, s3.bucket, minio.ListObjectsOptions{
			Prefix: prefix,
		}) {
			var id AuditID
			var err error
			if object.Err != nil {
				err = object.Err
			} else {
				id, err = ParseAuditID(strings.TrimPrefix(object.Key, prefix))
			}
			if !yield(id, err) {
				break
			}
		}
	}
}
