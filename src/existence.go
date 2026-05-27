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

type Existence int

const (
	ExistenceImpossible Existence = iota
	ExistencePossible
)

func (value Existence) IsImpossible() bool {
	return value == ExistenceImpossible
}

func (value Existence) IsPossible() bool {
	return value == ExistencePossible
}

func (value Existence) String() string {
	switch value {
	case ExistenceImpossible:
		return "impossible"
	case ExistencePossible:
		return "possible"
	default:
		return "(invalid)"
	}
}

type ExistenceCache interface {
	// Check if we might be serving the site.
	CheckSite(ctx context.Context, site string) (result Existence)

	// Check if we might be serving the domain.
	CheckDomain(ctx context.Context, domain string) (result Existence)

	// Add the site to the cache.
	AddSite(ctx context.Context, site string)
}

func CreateExistenceCache(ctx context.Context) (ExistenceCache, error) {
	if config.Feature("existence-cache") {
		switch config.Storage.Type {
		case "s3":
			maxAge := time.Duration(config.Storage.S3.SiteCache.MaxAge)
			return createBloomExistenceCache(ctx, maxAge)
		default:
			// not needed
		}
	}

	return &dummyExistenceCache{}, nil
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

func createBloomExistenceCache(ctx context.Context, maxAge time.Duration) (ExistenceCache, error) {
	cache := bloomExistenceCache{
		accessCh: make(chan struct{}),
		maxAge:   maxAge,
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
			logc.Println(ctx, "existence: refreshing")
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
		logc.Println(ctx, "existence: unchanged")
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

func (c *bloomExistenceCache) CheckSite(ctx context.Context, site string) (result Existence) {
	select {
	case c.accessCh <- struct{}{}:
	default:
	}

	c.filterMu.Lock()
	if c.sites.TestString(site) {
		result = ExistencePossible
	} else {
		result = ExistenceImpossible
	}
	c.filterMu.Unlock()

	logc.Printf(ctx, "existence: site %s: %s", site, result)
	return
}

func (c *bloomExistenceCache) CheckDomain(ctx context.Context, domain string) (result Existence) {
	select {
	case c.accessCh <- struct{}{}:
	default:
	}

	c.filterMu.Lock()
	if c.domains.TestString(domain) {
		result = ExistencePossible
	} else {
		result = ExistenceImpossible
	}
	c.filterMu.Unlock()

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

func (d dummyExistenceCache) CheckSite(context.Context, string) Existence {
	return ExistencePossible
}

func (d dummyExistenceCache) CheckDomain(context.Context, string) Existence {
	return ExistencePossible
}

func (d dummyExistenceCache) AddSite(context.Context, string) {}
