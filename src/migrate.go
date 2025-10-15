package git_pages

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
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
		log.Print("store already has domain markers")
		return nil
	}

	var manifests, domains []string
	manifests, err := backend.ListManifests(ctx)
	if err != nil {
		return fmt.Errorf("list manifests: %w", err)
	}
	slices.Sort(manifests)
	for _, manifest := range manifests {
		domain, _, _ := strings.Cut(manifest, "/")
		if len(domains) == 0 || domains[len(domains)-1] != domain {
			domains = append(domains, domain)
		}
	}
	for idx, domain := range domains {
		log.Printf("(%d / %d) creating domain %s", idx+1, len(domains), domain)
		if err := backend.CreateDomain(ctx, domain); err != nil {
			return fmt.Errorf("creating domain %s: %w", domain, err)
		}
	}
	if err := backend.EnableFeature(ctx, FeatureCheckDomainMarker); err != nil {
		return err
	}
	log.Printf("created markers for %d domains", len(domains))
	return nil
}
