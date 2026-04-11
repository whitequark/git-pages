package git_pages

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
)

type DomainCache interface {
	// Check if we might be serving the domain.
	CheckDomain(ctx context.Context, domain string) (found bool)

	// Add the domain to the cache.
	AddDomain(ctx context.Context, domain string)
}

func CreateDomainCache(ctx context.Context) (DomainCache, error) {
	if !config.Feature("domain-existence-cache") {
		return &dummyDomainCache{}, nil
	}
	return createBloomDomainCache(ctx)
}

type bloomDomainCache struct {
	filter   *bloom.BloomFilter
	filterMu sync.Mutex

	accessCh    chan struct{}
	refreshMu   sync.Mutex
	lastRefresh time.Time
	maxAge      time.Duration
}

func createBloomDomainCache(ctx context.Context) (DomainCache, error) {
	cache := bloomDomainCache{
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

func (c *bloomDomainCache) handleFilterUpdates(ctx context.Context) {
	for range c.accessCh {
		if time.Since(c.lastRefresh) > c.maxAge {
			logc.Print(ctx, "domain cache: refreshing")
			if err := c.refresh(ctx); err != nil {
				logc.Printf(ctx, "domain cache: refresh error: %v", err)
			}
		}
	}
}

func (c *bloomDomainCache) refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	if changed, err := backend.HaveDomainsChanged(ctx, c.lastRefresh); err != nil {
		return err
	} else if !changed {
		logc.Print(ctx, "domain cache: unchanged")
		c.lastRefresh = time.Now()
		return nil
	}

	// Create a 256 KiB Bloom filter that will fit ~150K entries with 0.1% false positive rate.
	filter := bloom.New(256*1024, 10)
	for metadata, err := range backend.EnumerateManifests(ctx) {
		if err != nil {
			return fmt.Errorf("enum manifests: %w", err)
		}
		domain, _, _ := strings.Cut(metadata.Name, "/")
		filter.AddString(domain)
	}

	c.filterMu.Lock()
	c.filter = filter
	c.filterMu.Unlock()

	logc.Printf(ctx, "domain cache: refreshed with approx. %d domains", filter.ApproximatedSize())
	c.lastRefresh = time.Now()
	return nil
}

func (c *bloomDomainCache) CheckDomain(ctx context.Context, domain string) (found bool) {
	select {
	case c.accessCh <- struct{}{}:
	default:
	}

	c.filterMu.Lock()
	found = c.filter.TestString(domain)
	c.filterMu.Unlock()

	logc.Printf(ctx, "domain cache: bloom filter returns %v for %q", found, domain)
	return
}

func (c *bloomDomainCache) AddDomain(ctx context.Context, domain string) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.filterMu.Lock()
	c.filter.AddString(domain)
	c.filterMu.Unlock()

	logc.Printf(ctx, "domain cache: added %q", domain)
}

type dummyDomainCache struct{}

func (d dummyDomainCache) CheckDomain(context.Context, string) bool { return true }

func (d dummyDomainCache) AddDomain(context.Context, string) {}
