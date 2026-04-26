package git_pages

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

type DomainStatistics struct {
	Domain         string
	OriginalSize   int64
	CompressedSize int64
	StoredSize     int64
}

func SizeHistogram(ctx context.Context) ([]*DomainStatistics, error) {
	statisticsMap := map[string]*DomainStatistics{}
	for item, err := range backend.GetAllManifests(ctx) {
		metadata, manifest := item.Splat()
		if err != nil {
			return nil, fmt.Errorf("size histogram err: %w", err)
		}
		domain, _, _ := strings.Cut(metadata.Name, "/")
		if _, found := statisticsMap[domain]; !found {
			statisticsMap[domain] = &DomainStatistics{Domain: domain}
		}
		statistics := statisticsMap[domain]
		statistics.OriginalSize += metadata.Size + manifest.GetOriginalSize()
		statistics.CompressedSize += metadata.Size + manifest.GetCompressedSize()
		statistics.StoredSize += metadata.Size + manifest.GetStoredSize()
	}
	return slices.Collect(maps.Values(statisticsMap)), nil
}
