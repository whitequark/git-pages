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
	exponential "github.com/jpillora/backoff"
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
	switch config.Storage.Type {
	case "s3":
		maxAge := time.Duration(config.Storage.S3.SiteCache.MaxAge)
		return createBloomExistenceCache(ctx, maxAge)
	default:
		// not needed
	}

	return &dummyExistenceCache{}, nil
}

type bloomExistenceCache struct {
	sites    *bloom.BloomFilter
	domains  *bloom.BloomFilter
	filterMu sync.Mutex

	accessCh    chan struct{}
	refreshMu   sync.Mutex
	lastChanged time.Time
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
	lastRefresh := time.Now()

	for range c.accessCh {
		if time.Since(lastRefresh) > c.maxAge {
			backoff := exponential.Backoff{
				Jitter: true,
				Min:    time.Second * 1,
				Max:    time.Second * 60,
			}
			for {
				logc.Println(ctx, "existence: refreshing")
				err := c.refreshIfChanged(ctx)
				if err == nil {
					lastRefresh = time.Now()
					break
				}
				sleepFor := backoff.Duration()
				logc.Printf(ctx, "existence: refresh error: %v (retry in %s)", err, sleepFor)
				time.Sleep(sleepFor)
			}
		}
	}
}

func (c *bloomExistenceCache) refreshIfChanged(ctx context.Context) error {
	changed, lastChanged, err := backend.HasSiteListChanged(ctx, c.lastChanged)
	if err != nil {
		return err
	} else if !changed {
		logc.Println(ctx, "existence: unchanged")
		return nil
	}

	err = c.refresh(ctx)
	if err != nil {
		return err
	}

	c.lastChanged = lastChanged
	return nil
}

func (c *bloomExistenceCache) refresh(ctx context.Context) error {
	// Create two 256 KiB Bloom filters that will fit ~150K entries each with 0.1% false positive rate.
	sites := bloom.New(256*1024*8, 10)
	domains := bloom.New(256*1024*8, 10)

	// To prevent sites created during enumeration from getting dropped, acquire an exclusive lock
	// so that filter additions wait until we finish enumerating sites and update the filters.
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

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

	logc.Debugf(ctx, "existence: site %s: %s", site, result)
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

	logc.Debugf(ctx, "existence: domain %s: %s", domain, result)
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
