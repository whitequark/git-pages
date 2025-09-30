package main

import (
	"context"
	"io"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	blobsRetrievedCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blobs_retrieved",
		Help: "Count of blobs retrieved",
	})
	blobsRetrievedBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blobs_retrieved_bytes",
		Help: "Total size in bytes of blobs retrieved",
	})

	blobsStoredCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blobs_stored",
		Help: "Count of blobs stored",
	})
	blobsStoredBytes = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_blobs_stored_bytes",
		Help: "Total size in bytes of blobs stored",
	})

	manifestsRetrievedCount = promauto.NewCounter(prometheus.CounterOpts{
		Name: "git_pages_manifests_retrieved",
		Help: "Count of manifests retrieved",
	})
)

type observedBackend struct {
	inner Backend
}

var _ Backend = (*observedBackend)(nil)

func NewObservedBackend(backend Backend) Backend {
	return &observedBackend{inner: backend}
}

func (backend *observedBackend) GetBlob(
	ctx context.Context,
	name string,
) (
	reader io.ReadSeeker,
	size uint64,
	mtime time.Time,
	err error,
) {
	span, ctx := ObserveFunction(ctx, "GetBlob", "blob.name", name)
	if reader, size, mtime, err = backend.inner.GetBlob(ctx, name); err == nil {
		ObserveData(ctx, "blob.size", size)
		blobsRetrievedCount.Inc()
		blobsRetrievedBytes.Add(float64(size))
	}
	span.Finish()
	return
}

func (backend *observedBackend) PutBlob(ctx context.Context, name string, data []byte) (err error) {
	span, ctx := ObserveFunction(ctx, "PutBlob", "blob.name", name, "blob.size", len(data))
	if err = backend.inner.PutBlob(ctx, name, data); err == nil {
		blobsStoredCount.Inc()
		blobsStoredBytes.Add(float64(len(data)))
	}
	span.Finish()
	return
}

func (backend *observedBackend) DeleteBlob(ctx context.Context, name string) (err error) {
	span, ctx := ObserveFunction(ctx, "DeleteBlob", "blob.name", name)
	err = backend.inner.DeleteBlob(ctx, name)
	span.Finish()
	return
}

func (backend *observedBackend) GetManifest(ctx context.Context, name string) (manifest *Manifest, err error) {
	span, ctx := ObserveFunction(ctx, "GetManifest", "manifest.name", name)
	if manifest, err = backend.inner.GetManifest(ctx, name); err == nil {
		manifestsRetrievedCount.Inc()
	}
	span.Finish()
	return
}

func (backend *observedBackend) StageManifest(ctx context.Context, manifest *Manifest) (err error) {
	span, ctx := ObserveFunction(ctx, "StageManifest")
	err = backend.inner.StageManifest(ctx, manifest)
	span.Finish()
	return
}

func (backend *observedBackend) CommitManifest(ctx context.Context, name string, manifest *Manifest) (err error) {
	span, ctx := ObserveFunction(ctx, "CommitManifest", "manifest.name", name)
	err = backend.inner.CommitManifest(ctx, name, manifest)
	span.Finish()
	return
}

func (backend *observedBackend) DeleteManifest(ctx context.Context, name string) (err error) {
	span, ctx := ObserveFunction(ctx, "DeleteManifest", "manifest.name", name)
	err = backend.inner.DeleteManifest(ctx, name)
	span.Finish()
	return
}

func (backend *observedBackend) CheckDomain(ctx context.Context, domain string) (found bool, err error) {
	span, ctx := ObserveFunction(ctx, "CheckDomain", "manifest.domain", domain)
	found, err = backend.inner.CheckDomain(ctx, domain)
	span.Finish()
	return
}
