package git_pages

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
)

type SiteExistenceCache interface {
	// Check if we might be serving the site.
	CheckSite(ctx context.Context, site string) (found bool)

	// Check if we might be serving the domain.
	CheckDomain(ctx context.Context, domain string) (found bool)

	// Add the site to the cache.
	AddSite(ctx context.Context, site string)
}

func CreateSiteExistenceCache(ctx context.Context) (SiteExistenceCache, error) {
	if !config.Feature("site-existence-cache") {
		return &dummySiteExistenceCache{}, nil
	}
	return createBloomSiteExistenceCache(ctx)
}

type bloomSiteExistenceCache struct {
	sites    *bloom.BloomFilter
	domains  *bloom.BloomFilter
	filterMu sync.Mutex

	accessCh    chan struct{}
	refreshMu   sync.Mutex
	lastRefresh time.Time
	maxAge      time.Duration
}

func createBloomSiteExistenceCache(ctx context.Context) (SiteExistenceCache, error) {
	cache := bloomSiteExistenceCache{
		accessCh: make(chan struct{}),
	}

	switch config.Storage.Type {
	case "fs":
		// the FS backend has no cache
	case "s3":
		cache.maxAge = time.Duration(config.Storage.S3.SiteCache.MaxAge)
	default:
		panic(fmt.Errorf("unknown backend: %s", config.Storage.Type))
	}

	if err := cache.refresh(ctx); err != nil {
		return nil, err
	}

	go cache.handleFilterUpdates(ctx)

	return &cache, nil
}

func (c *bloomSiteExistenceCache) handleFilterUpdates(ctx context.Context) {
	for range c.accessCh {
		if time.Since(c.lastRefresh) > c.maxAge {
			logc.Print(ctx, "site existence cache: refreshing")
			if err := c.refresh(ctx); err != nil {
				logc.Printf(ctx, "site existence cache: refresh error: %v", err)
			}
		}
	}
}

func (c *bloomSiteExistenceCache) refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	if changed, err := backend.HasSiteListChanged(ctx, c.lastRefresh); err != nil {
		return err
	} else if !changed {
		logc.Print(ctx, "site existence cache: unchanged")
		c.lastRefresh = time.Now()
		return nil
	}

	var siteCount int
	// Create two 256 KiB Bloom filters that will fit ~150K entries each with 0.1% false positive rate.
	sites := bloom.New(256*1024, 10)
	domains := bloom.New(256*1024, 10)
	for metadata, err := range backend.EnumerateManifests(ctx) {
		if err != nil {
			return fmt.Errorf("enum manifests: %w", err)
		}
		site := metadata.Name
		domain, _, _ := strings.Cut(site, "/")
		sites.AddString(site)
		domains.AddString(domain)
		siteCount++
	}

	c.filterMu.Lock()
	c.sites = sites
	c.domains = domains
	c.filterMu.Unlock()

	logc.Printf(ctx, "site existence cache: refreshed with %d sites", siteCount)
	c.lastRefresh = time.Now()
	return nil
}

func (c *bloomSiteExistenceCache) CheckSite(ctx context.Context, site string) (found bool) {
	select {
	case c.accessCh <- struct{}{}:
	default:
	}

	c.filterMu.Lock()
	found = c.sites.TestString(site)
	c.filterMu.Unlock()

	logc.Printf(ctx, "site existence cache: bloom filter returns %v for site %q", found, site)
	return
}

func (c *bloomSiteExistenceCache) CheckDomain(ctx context.Context, domain string) (found bool) {
	select {
	case c.accessCh <- struct{}{}:
	default:
	}

	c.filterMu.Lock()
	found = c.domains.TestString(domain)
	c.filterMu.Unlock()

	logc.Printf(ctx, "site existence cache: bloom filter returns %v for domain %q", found, domain)
	return
}

func (c *bloomSiteExistenceCache) AddSite(ctx context.Context, site string) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	domain, _, _ := strings.Cut(site, "/")

	c.filterMu.Lock()
	c.sites.AddString(site)
	c.domains.AddString(domain)
	c.filterMu.Unlock()

	logc.Printf(ctx, "site existence cache: added site %q", site)
}

type dummySiteExistenceCache struct{}

func (d dummySiteExistenceCache) CheckSite(context.Context, string) bool { return true }

func (d dummySiteExistenceCache) CheckDomain(context.Context, string) bool { return true }

func (d dummySiteExistenceCache) AddSite(context.Context, string) {}
