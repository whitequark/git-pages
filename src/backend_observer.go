package main

import (
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
	backend Backend
}

func NewObservedBackend(backend Backend) Backend {
	return &observedBackend{backend: backend}
}

func (b *observedBackend) GetBlob(name string) (reader io.ReadSeeker, size uint64, mtime time.Time, err error) {
	reader, size, mtime, err = b.backend.GetBlob(name)
	if err != nil {
		return
	}
	blobsRetrievedCount.Inc()
	blobsRetrievedBytes.Add(float64(size))
	return
}

func (b *observedBackend) PutBlob(name string, data []byte) error {
	err := b.backend.PutBlob(name, data)
	if err != nil {
		return err
	}
	blobsStoredCount.Inc()
	blobsStoredBytes.Add(float64(len(data)))
	return nil
}

func (b *observedBackend) DeleteBlob(name string) error {
	return b.backend.DeleteBlob(name)
}

func (b *observedBackend) GetManifest(name string) (manifest *Manifest, err error) {
	manifest, err = b.backend.GetManifest(name)
	if err != nil {
		return
	}
	manifestsRetrievedCount.Inc()
	return
}

func (b *observedBackend) StageManifest(manifest *Manifest) error {
	return b.backend.StageManifest(manifest)
}

func (b *observedBackend) CommitManifest(name string, manifest *Manifest) error {
	return b.backend.CommitManifest(name, manifest)
}

func (b *observedBackend) DeleteManifest(name string) error {
	return b.backend.DeleteManifest(name)
}

func (b *observedBackend) CheckDomain(domain string) (found bool, err error) {
	return b.backend.CheckDomain(domain)
}
