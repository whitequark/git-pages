// The existence cache allows fast rejection of requests for nonexistent domains or sites.
// This is principally important for floods of crawler requests which probe either random
// domains (with A/AAAA records not pointing to the git-pages host), or random sites
// (typically as a result of probing for vulnerable URLs). With the S3 backend this can
// result in severe congestion of the backend channel and high CPU use.

package git_pages

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
)

type ExistenceCache interface {
	// Check if we might be serving the site.
	CheckSite(ctx context.Context, site string) (found bool)

	// Check if we might be serving the domain.
	CheckDomain(ctx context.Context, domain string) (found bool)

	// Add the site to the cache.
	AddSite(ctx context.Context, site string)
}

func CreateExistenceCache(ctx context.Context) (ExistenceCache, error) {
	if !config.Feature("existence-cache") {
		return &dummyExistenceCache{}, nil
	}
	return createBloomExistenceCache(ctx)
}

type bloomExistenceCache struct {
	sites    *bloom.BloomFilter
	domains  *bloom.BloomFilter
	filterMu sync.Mutex

	accessCh    chan struct{}
	refreshMu   sync.Mutex
	lastRefresh time.Time
	maxAge      time.Duration
}

func createBloomExistenceCache(ctx context.Context) (ExistenceCache, error) {
	cache := bloomExistenceCache{
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

func (c *bloomExistenceCache) handleFilterUpdates(ctx context.Context) {
	for range c.accessCh {
		if time.Since(c.lastRefresh) > c.maxAge {
			logc.Print(ctx, "existence: refreshing")
			if err := c.refresh(ctx); err != nil {
				logc.Printf(ctx, "existence: refresh error: %v", err)
			}
		}
	}
}

func (c *bloomExistenceCache) refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	if changed, err := backend.HasSiteListChanged(ctx, c.lastRefresh); err != nil {
		return err
	} else if !changed {
		logc.Print(ctx, "existence: unchanged")
		c.lastRefresh = time.Now()
		return nil
	}

	// Create two 256 KiB Bloom filters that will fit ~150K entries each with 0.1% false positive rate.
	sites := bloom.New(256*1024, 10)
	domains := bloom.New(256*1024, 10)

	logc.Printf(ctx, "existence: refreshing")
	siteCount := 0
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

	logc.Printf(ctx, "existence: refreshed with %d sites", siteCount)
	c.lastRefresh = time.Now()
	return nil
}

func (c *bloomExistenceCache) CheckSite(ctx context.Context, site string) (found bool) {
	select {
	case c.accessCh <- struct{}{}:
	default:
	}

	c.filterMu.Lock()
	found = c.sites.TestString(site)
	c.filterMu.Unlock()

	result := "miss"
	if found {
		result = "hit"
	}
	logc.Printf(ctx, "existence: site %s: %s", site, result)
	return
}

func (c *bloomExistenceCache) CheckDomain(ctx context.Context, domain string) (found bool) {
	select {
	case c.accessCh <- struct{}{}:
	default:
	}

	c.filterMu.Lock()
	found = c.domains.TestString(domain)
	c.filterMu.Unlock()

	result := "miss"
	if found {
		result = "hit"
	}
	logc.Printf(ctx, "existence: domain %s: %s", domain, result)
	return
}

func (c *bloomExistenceCache) AddSite(ctx context.Context, site string) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	domain, _, _ := strings.Cut(site, "/")

	c.filterMu.Lock()
	c.sites.AddString(site)
	c.domains.AddString(domain)
	c.filterMu.Unlock()

	logc.Printf(ctx, "existence: added site %s", site)
}

type dummyExistenceCache struct{}

func (d dummyExistenceCache) CheckSite(context.Context, string) bool { return true }

func (d dummyExistenceCache) CheckDomain(context.Context, string) bool { return true }

func (d dummyExistenceCache) AddSite(context.Context, string) {}
