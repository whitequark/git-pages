package git_pages

import (
	"context"
	"fmt"
	"strings"

	"codeberg.org/git-pages/git-pages/src/names"
)

type StorageSize struct {
	Domain string `json:"domain"`
	// Size of live storage for the current versions of sites.
	CurrentSize int64 `json:"currentSize"`
	// Size of live blobs (only) for the current version of sites.
	CurrentBlobSize int64 `json:"-"`
	// Size of live storage for the non-current versions of sites in audit records.
	NonCurrentSize int64 `json:"nonCurrentSize"`
	// Total size of live storage for this domain.
	TotalSize int64 `json:"totalSize"`
}

func AnalyzeStorage(ctx context.Context) ([]*StorageSize, error) {
	type storageData struct {
		siteManifests int64
		siteBlobs     map[string]int64
		auditRecords  int64
		auditBlobs    map[string]int64
	}

	thickStats := map[names.Domain]*storageData{}
	thinStats := []*StorageSize{}

	extraSuffixes := []names.Domain{}
	for _, wildcard := range wildcards {
		extraSuffixes = append(extraSuffixes,
			Must(names.ParseDomain(strings.Join(wildcard.Domain, "."), names.ParseDomainRelaxed)))
		if len(wildcard.PreviewDomain) > 0 {
			extraSuffixes = append(extraSuffixes,
				Must(names.ParseDomain(strings.Join(wildcard.PreviewDomain, "."), names.ParseDomainRelaxed)))
		}
	}

	getStats := func(domain names.Domain) *storageData {
		if domain.IsValid() {
			domain = domain.EffectiveTLDPlusOne(extraSuffixes)
		}
		if _, found := thickStats[domain]; !found {
			thickStats[domain] = &storageData{
				siteBlobs:  map[string]int64{},
				auditBlobs: map[string]int64{},
			}
		}
		return thickStats[domain]
	}

	totalStats := getStats(names.Domain{})

	logc.Printf(ctx, "analyze: enumerating manifests")
	for item, err := range backend.GetAllManifests(ctx) {
		metadata, manifest := item.Splat()
		if err != nil {
			return nil, fmt.Errorf("analyze err: %w", err)
		}
		stats := getStats(metadata.Name.Domain)
		stats.siteManifests += metadata.Size
		totalStats.siteManifests += metadata.Size
		for _, entry := range manifest.GetContents() {
			if entry.GetType() == Type_ExternalFile {
				blobName, blobSize := string(entry.Data), entry.GetCompressedSize()
				stats.siteBlobs[blobName] = blobSize
				totalStats.siteBlobs[blobName] = blobSize
			}
		}
	}

	logc.Printf(ctx, "analyze: enumerating audit records")
	auditIDs := backend.SearchAuditLog(ctx, SearchAuditLogOptions{})
	for record, err := range backend.GetAuditLogRecords(ctx, auditIDs) {
		if err != nil {
			return nil, fmt.Errorf("analyze err: %w", err)
		}
		domain := Must(names.ParseDomain(record.GetDomain(), names.ParseDomainRelaxed))
		stats := getStats(domain)
		recordSize := int64(len(EncodeAuditRecord(record)))
		stats.auditRecords += recordSize
		totalStats.auditRecords += recordSize
		if record.Manifest == nil || record.IsDetached() {
			continue
		}
		for _, entry := range record.Manifest.GetContents() {
			if entry.GetType() == Type_ExternalFile {
				blobName, blobSize := string(entry.Data), entry.GetCompressedSize()
				if _, found := stats.siteBlobs[blobName]; found {
					continue // already accounted for
				}
				stats.auditBlobs[blobName] = entry.GetCompressedSize()
				totalStats.auditBlobs[blobName] = blobSize
			}
		}
	}

	// Now aggregate the information.
	for domain, stats := range thickStats {
		sizes := StorageSize{Domain: domain.String()}
		sizes.CurrentSize += stats.siteManifests
		for _, size := range stats.siteBlobs {
			sizes.CurrentSize += size
			sizes.CurrentBlobSize += size
		}
		sizes.NonCurrentSize += stats.auditRecords
		for _, size := range stats.auditBlobs {
			sizes.NonCurrentSize += size
		}
		sizes.TotalSize = sizes.CurrentSize + sizes.NonCurrentSize
		thinStats = append(thinStats, &sizes)
	}

	return thinStats, nil
}
