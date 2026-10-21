package git_pages

import (
	"context"
	"fmt"
	"slices"

	"codeberg.org/git-pages/git-pages/src/names"
)

func RunMigration(ctx context.Context, name string) error {
	switch name {
	case "create-domain-markers":
		return createDomainMarkers(ctx)
	default:
		return fmt.Errorf("unknown migration name (expected one of \"create-domain-markers\")")
	}
}

func createDomainMarkers(ctx context.Context) error {
	if backend.HasFeature(ctx, FeatureCheckDomainMarker) {
		logc.Println(ctx, "store already has domain markers")
		return nil
	}

	var sites []names.Site
	for metadata, err := range backend.EnumerateManifests(ctx) {
		if err != nil {
			return fmt.Errorf("enum manifests: %w", err)
		}
		sites = append(sites, metadata.Name)
	}
	slices.SortFunc(sites, names.CompareSite)

	var domains []names.Domain
	for _, site := range sites {
		if len(domains) == 0 || domains[len(domains)-1] != site.Domain {
			domains = append(domains, site.Domain)
		}
	}
	for idx, domain := range domains {
		logc.Printf(ctx, "(%d / %d) creating domain %s", idx+1, len(domains), domain)
		if err := backend.CreateDomain(ctx, domain); err != nil {
			return fmt.Errorf("creating domain %s: %w", domain, err)
		}
	}
	if err := backend.EnableFeature(ctx, FeatureCheckDomainMarker); err != nil {
		return err
	}

	logc.Printf(ctx, "created markers for %d domains", len(domains))
	return nil
}
