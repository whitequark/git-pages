package git_pages

import (
	"context"
	"errors"
	"fmt"
	"net/http"

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

func RepairStorage(ctx context.Context, dryRun bool) (bool, error) {
	var corruptedManifests, corruptedAuditRecords uint

	dryRunSuffix := ""
	if dryRun {
		dryRunSuffix = " (dry run)"
	}

	logc.Println(ctx, "repair: checking manifests"+dryRunSuffix)
	for metadata, err := range backend.EnumerateManifests(ctx) {
		if err != nil {
			// Error fetching metadata, can't repair that.
			return false, fmt.Errorf("repair err: %w", err)
		}

		_, _, err = backend.GetManifest(ctx, metadata.Name, GetManifestOptions{})
		if errors.Is(err, proto.Error) {
			logc.Printf(ctx, "repair fix: %s\n", err)
			corruptedManifests += 1

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
	}

	// Enumerate blobs live via audit records.
	logc.Println(ctx, "repair: checking audit records"+dryRunSuffix)
	for auditID, err := range backend.SearchAuditLog(ctx, SearchAuditLogOptions{}) {
		if err != nil {
			// Error fetching metadata, can't repair that.
			return false, fmt.Errorf("repair err: %w", err)
		}

		_, err = backend.QueryAuditLog(ctx, auditID)
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
		}
	}

	if dryRun {
		logc.Printf(ctx, "repair: %d manifests corrupt", corruptedManifests)
		logc.Printf(ctx, "repair: %d audit records corrupt", corruptedAuditRecords)
	} else {
		logc.Printf(ctx, "repair: %d manifests repaired", corruptedManifests)
		logc.Printf(ctx, "repair: %d audit records repaired", corruptedAuditRecords)
	}

	allGood := corruptedManifests == 0 && corruptedAuditRecords == 0
	return allGood, nil
}
