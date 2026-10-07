package git_pages

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	timestamppb "google.golang.org/protobuf/types/known/timestamppb"
)

func createPlaceholderManifest(ctx context.Context, name string) *Manifest {
	manifest := NewManifest()
	manifest.Corrupted = proto.Bool(true)
	AddFile(manifest, "index.html",
		[]byte("<!DOCTYPE html>"+
			"<h1>Data corruption</h1>"+
			"<p>The site <code>"+name+"</code> has suffered data corruption and must be "+
			"re-uploaded. Contact the server administrator for more information.</p>"),
	)
	manifest.Redirects = []*RedirectRule{
		{
			From: proto.String("/*"),
			To:   proto.String("/index.html"),
			// Status 500 cannot be used in a `_redirects` file, but we can create such a redirect
			// rule manually.
			Status: proto.Uint32(http.StatusInternalServerError),
			Force:  proto.Bool(true),
		},
	}
	return manifest
}

func createPlaceholderAuditRecord(ctx context.Context, id AuditID) *AuditRecord {
	record := &AuditRecord{Event: AuditEvent_CorruptedEvent.Enum()}
	record.Id = proto.Int64(int64(id))
	record.Timestamp = timestamppb.Now()
	record.Principal = GetPrincipal(ctx)
	if reason := GetReason(ctx); reason != nil && *reason != "" {
		record.Reason = GetReason(ctx)
	}
	return record
}

type blobIndex struct {
	nameSet map[string]struct{}
	refTime time.Time
}

// There are two main factors which may cause a manifest to look like it is missing a blob when
// in fact it does not: eventual consistency in a distributed system, and TOCTTOU delay when
// racing an upload. The second factor is the primary one, as every reasonably active deployment
// quickly reaches the point where at least one upload will happen while the blobs are being traced.
//
// Our solution to both is the same: to avoid making conclusions about recently updated manifests
// and audit records. Note that "recently updated" has to be computed relative to the _start_ of
// the blob enumeration process. This way, any manifests uploaded after the first blob is enumerated
// will be considered "recently updated" regardless of how long the enumeration process lasts.
const clockSkewTolerance time.Duration = 60 * time.Minute

func createBlobIndex() *blobIndex {
	return &blobIndex{nameSet: map[string]struct{}{}, refTime: time.Now()}
}

func (index *blobIndex) IsFresh(lastModified time.Time) bool {
	return lastModified.Add(clockSkewTolerance).Before(index.refTime)
}

func (index *blobIndex) Add(metadata BlobMetadata) {
	index.nameSet[metadata.Name] = struct{}{}
}

func (index *blobIndex) Has(name string) bool {
	_, found := index.nameSet[name]
	return found
}

type repairObject struct {
	Manifest     *Manifest
	LastModified time.Time
	ObjectName   string
}

func hasDanglingBlobReferences(
	ctx context.Context, blobIndex *blobIndex, repairManifest *repairObject,
) bool {
	if !blobIndex.IsFresh(repairManifest.LastModified) {
		// We can't evaluate this manifest, it is too recent.
		return false
	}

	hasDangling := false
	for _, entry := range repairManifest.Manifest.GetContents() {
		if entry.GetType() == Type_ExternalFile {
			blobName := string(entry.GetData())
			if !blobIndex.Has(blobName) {
				logc.Printf(ctx, "repair fix: %s: dangling reference %s",
					repairManifest.ObjectName, blobName)
				hasDangling = true
				// Continue printing dangling reference names.
			}
		}
	}
	return hasDangling
}

func RepairStorage(ctx context.Context, dryRun bool) (bool, error) {
	dryRunSuffix := ""
	if dryRun {
		dryRunSuffix = " (dry run)"
	}

	var corruptedBlobs, corruptedManifests, corruptedAuditRecords uint
	blobIndex := createBlobIndex() // captures `time.Now()` as reference time

	logc.Println(ctx, "repair: checking blobs"+dryRunSuffix)
	for metadata, err := range backend.EnumerateBlobs(ctx) {
		if err != nil {
			// Error fetching metadata, can't repair that.
			return false, fmt.Errorf("repair err: %w", err)
		}

		blobReader, _, err := backend.GetBlob(ctx, metadata.Name)
		if err != nil {
			// Error fetching data, can't repair that.
			return false, fmt.Errorf("repair err: %w", err)
		}

		blobData, err := io.ReadAll(blobReader)
		if closer, ok := blobReader.(io.Closer); ok {
			closer.Close()
		}
		if err != nil {
			// As above.
			return false, fmt.Errorf("repair err: %w", err)
		}

		if blobHash, ok := strings.CutPrefix(metadata.Name, "sha256-"); ok {
			if blobHash != fmt.Sprintf("%x", sha256.Sum256(blobData)) {
				logc.Printf(ctx, "repair fix: blob hash mismatch: %s\n", metadata.Name)
				corruptedBlobs += 1

				if !dryRun {
					err = backend.DeleteBlob(ctx, metadata.Name)
					if err != nil {
						return false, fmt.Errorf("repair err: delete blob: %w", err)
					}
				}
			} else {
				blobIndex.Add(metadata)
			}
		} else {
			return false, fmt.Errorf("repair err: invalid blob name: %s", metadata.Name)
		}
	}

	logc.Println(ctx, "repair: checking manifests"+dryRunSuffix)
	for metadata, err := range backend.EnumerateManifests(ctx) {
		if err != nil {
			// Error fetching metadata, can't repair that.
			return false, fmt.Errorf("repair err: %w", err)
		}

		manifest, _, err := backend.GetManifest(ctx, metadata.Name, GetManifestOptions{})
		if errors.Is(err, proto.Error) {
			logc.Printf(ctx, "repair fix: %s\n", err)
			corruptedManifests += 1
		} else if hasDanglingBlobReferences(ctx, blobIndex, &repairObject{
			Manifest:     manifest,
			LastModified: metadata.LastModified,
			ObjectName:   fmt.Sprintf("site/%s", metadata.Name),
		}) {
			corruptedManifests += 1
		} else {
			continue
		}

		if !dryRun {
			manifest := createPlaceholderManifest(ctx, metadata.Name)
			err = backend.StageManifest(ctx, manifest)
			if err != nil {
				return false, fmt.Errorf("repair err: stage manifest: %w", err)
			}
			err = backend.CommitManifest(ctx, metadata.Name, manifest, ModifyManifestOptions{})
			if err != nil {
				return false, fmt.Errorf("repair err: commit manifest: %w", err)
			}
		}
	}

	// Enumerate blobs live via audit records.
	logc.Println(ctx, "repair: checking audit records"+dryRunSuffix)
	for auditID, err := range backend.SearchAuditLog(ctx, SearchAuditLogOptions{}) {
		if err != nil {
			// Error fetching metadata, can't repair that.
			return false, fmt.Errorf("repair err: %w", err)
		}

		auditRecord, err := backend.QueryAuditLog(ctx, auditID)
		if errors.Is(err, proto.Error) {
			logc.Printf(ctx, "repair fix: %s\n", err)
			corruptedAuditRecords += 1

			if !dryRun {
				// In general, you're not supposed to do this, but repair is an exception.
				auditRecord := createPlaceholderAuditRecord(ctx, auditID)
				err = backend.ExpireAuditRecord(ctx, auditID)
				if err != nil {
					return false, fmt.Errorf("repair err: expire audit: %w", err)
				}
				err = backend.AppendAuditLog(ctx, auditID, auditRecord)
				if err != nil {
					return false, fmt.Errorf("repair err: append audit: %w", err)
				}
			}
		} else if hasDanglingBlobReferences(ctx, blobIndex, &repairObject{
			Manifest:     auditRecord.GetManifest(),
			LastModified: auditRecord.GetTimestamp().AsTime(),
			ObjectName:   fmt.Sprintf("audit/%s", auditID),
		}) {
			corruptedAuditRecords += 1

			if !dryRun {
				err = backend.DetachAuditRecord(ctx, auditID)
				if err != nil {
					return false, fmt.Errorf("repair err: detach audit: %w", err)
				}
			}
		}
	}

	if dryRun {
		logc.Printf(ctx, "repair: %d blobs corrupt", corruptedManifests)
		logc.Printf(ctx, "repair: %d manifests corrupt", corruptedManifests)
		logc.Printf(ctx, "repair: %d audit records corrupt", corruptedAuditRecords)
	} else {
		logc.Printf(ctx, "repair: %d blobs repaired", corruptedBlobs)
		logc.Printf(ctx, "repair: %d manifests repaired", corruptedManifests)
		logc.Printf(ctx, "repair: %d audit records repaired", corruptedAuditRecords)
	}

	allGood := corruptedManifests == 0 && corruptedAuditRecords == 0
	return allGood, nil
}
