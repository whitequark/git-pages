package git_pages

import (
	"context"
	"fmt"

	"github.com/c2h5oh/datasize"
	"github.com/dghubble/trie"
)

func trieReduce(data trie.Trier) (items, total int64) {
	data.Walk(func(key string, value any) error {
		items += 1
		total += *value.(*int64)
		return nil
	})
	return
}

func TraceGarbage(ctx context.Context) error {
	allBlobs := trie.NewRuneTrie()
	liveBlobs := trie.NewRuneTrie()

	traceManifest := func(manifestName string, manifest *Manifest) error {
		for _, entry := range manifest.GetContents() {
			if entry.GetType() == Type_ExternalFile {
				blobName := string(entry.Data)
				if size := allBlobs.Get(blobName); size == nil {
					return fmt.Errorf("%s: dangling reference %s", manifestName, blobName)
				} else {
					liveBlobs.Put(blobName, size)
				}
			}
		}
		return nil
	}

	// Enumerate all blobs.
	for metadata, err := range backend.EnumerateBlobs(ctx) {
		if err != nil {
			return fmt.Errorf("trace blobs err: %w", err)
		}
		allBlobs.Put(metadata.Name, &metadata.Size)
	}

	// Enumerate blobs live via site manifests.
	for metadata, err := range backend.EnumerateManifests(ctx) {
		if err != nil {
			return fmt.Errorf("trace sites err: %w", err)
		}
		manifest, _, err := backend.GetManifest(ctx, metadata.Name, GetManifestOptions{})
		if err != nil {
			return fmt.Errorf("trace sites err: %w", err)
		}
		err = traceManifest(metadata.Name, manifest)
		if err != nil {
			return fmt.Errorf("trace sites err: %w", err)
		}
	}

	// Enumerate blobs live via audit records.
	for auditID, err := range backend.SearchAuditLog(ctx, SearchAuditLogOptions{}) {
		if err != nil {
			return fmt.Errorf("trace audit err: %w", err)
		}
		auditRecord, err := backend.QueryAuditLog(ctx, auditID)
		if err != nil {
			return fmt.Errorf("trace audit err: %w", err)
		}
		if auditRecord.Manifest != nil {
			err = traceManifest(auditID.String(), auditRecord.Manifest)
			if err != nil {
				return fmt.Errorf("trace audit err: %w", err)
			}
		}
	}

	allBlobsCount, allBlobsSize := trieReduce(allBlobs)
	logc.Printf(ctx, "trace all: %d blobs, %s",
		allBlobsCount, datasize.ByteSize(allBlobsSize).HR())

	liveBlobsCount, liveBlobsSize := trieReduce(liveBlobs)
	logc.Printf(ctx, "trace live: %d blobs, %s",
		liveBlobsCount, datasize.ByteSize(liveBlobsSize).HR())

	return nil
}
