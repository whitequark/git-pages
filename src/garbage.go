package git_pages

import (
	"context"
	"fmt"

	"github.com/c2h5oh/datasize"
)

func TraceGarbage(ctx context.Context) error {
	allBlobs := map[string]int64{}
	liveBlobs := map[string]int64{}

	reduceBlobs := func(data map[string]int64) (items, total int64) {
		for _, value := range data {
			items += 1
			total += value
		}
		return
	}

	traceManifest := func(manifestKind string, manifestName string, manifest *Manifest) error {
		for _, entry := range manifest.GetContents() {
			if entry.GetType() == Type_ExternalFile {
				blobName := string(entry.Data)
				if size, ok := allBlobs[blobName]; ok {
					liveBlobs[blobName] = size
				} else {
					logc.Printf(ctx, "trace manifest: %s/%s: dangling reference %s",
						manifestKind, manifestName, blobName)
				}
			}
		}
		return nil
	}

	// Enumerate all blobs.
	logc.Printf(ctx, "trace: enumerating blobs")
	for metadata, err := range backend.EnumerateBlobs(ctx) {
		if err != nil {
			return fmt.Errorf("trace blobs err: %w", err)
		}
		allBlobs[metadata.Name] = metadata.Size
	}

	// Enumerate blobs live via site manifests.
	logc.Printf(ctx, "trace: enumerating manifests")
	for item, err := range backend.GetAllManifests(ctx) {
		metadata, manifest := item.Splat()
		if err != nil {
			return fmt.Errorf("trace sites err: %w", err)
		}
		err = traceManifest("site", metadata.Name, manifest)
		if err != nil {
			return fmt.Errorf("trace sites err: %w", err)
		}
	}

	// Enumerate blobs live via audit records.
	logc.Printf(ctx, "trace: enumerating audit records")
	auditIDs := backend.SearchAuditLog(ctx, SearchAuditLogOptions{})
	for record, err := range backend.GetAuditLogRecords(ctx, auditIDs) {
		if err != nil {
			return fmt.Errorf("trace audit err: %w", err)
		}
		if record.Manifest != nil {
			err = traceManifest("audit", record.GetAuditID().String(), record.Manifest)
			if err != nil {
				return fmt.Errorf("trace audit err: %w", err)
			}
		}
	}

	allBlobsCount, allBlobsSize := reduceBlobs(allBlobs)
	liveBlobsCount, liveBlobsSize := reduceBlobs(liveBlobs)
	logc.Printf(ctx, "trace all: %d blobs, %s",
		allBlobsCount, datasize.ByteSize(allBlobsSize).HR())
	logc.Printf(ctx, "trace live: %d blobs, %s",
		liveBlobsCount, datasize.ByteSize(liveBlobsSize).HR())
	logc.Printf(ctx, "trace dead: %d blobs, %s",
		allBlobsCount-liveBlobsCount, datasize.ByteSize(allBlobsSize-liveBlobsSize).HR())

	return nil
}
