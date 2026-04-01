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
	for metadata, err := range backend.EnumerateManifests(ctx) {
		if err != nil {
			return nil, fmt.Errorf("size histogram err: %w", err)
		}
		manifest, _, err := backend.GetManifest(ctx, metadata.Name, GetManifestOptions{})
		if err != nil {
			return nil, fmt.Errorf("size histogram err: %w", err)
		}
		domain, _, _ := strings.Cut(metadata.Name, "/")
		if _, found := statisticsMap[domain]; !found {
			statisticsMap[domain] = &DomainStatistics{Domain: domain}
		}
		statistics := statisticsMap[domain]
		statistics.OriginalSize += manifest.GetOriginalSize()
		statistics.CompressedSize += manifest.GetCompressedSize()
		statistics.StoredSize += manifest.GetStoredSize()
	}
	return slices.Collect(maps.Values(statisticsMap)), nil
}
