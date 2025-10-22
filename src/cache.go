package git_pages

import (
	"context"
	"time"

	"github.com/maypok86/otter/v2"
	"github.com/prometheus/client_golang/prometheus"
)

type weightedCacheEntry interface {
	Weight() uint32
}

type trackedLoader[K comparable, V any] struct {
	loader   otter.Loader[K, V]
	loaded   bool
	reloaded bool
}

func (l *trackedLoader[K, V]) Load(ctx context.Context, key K) (V, error) {
	val, err := l.loader.Load(ctx, key)
	l.loaded = true
	return val, err
}

func (l *trackedLoader[K, V]) Reload(ctx context.Context, key K, oldValue V) (V, error) {
	val, err := l.loader.Reload(ctx, key, oldValue)
	l.reloaded = true
	return val, err
}

type observedCacheMetrics struct {
	HitNumberCounter      prometheus.Counter
	HitWeightCounter      prometheus.Counter
	MissNumberCounter     prometheus.Counter
	MissWeightCounter     prometheus.Counter
	EvictionNumberCounter prometheus.Counter
	EvictionWeightCounter prometheus.Counter
}

type observedCache[K comparable, V weightedCacheEntry] struct {
	Cache *otter.Cache[K, V]

	metrics observedCacheMetrics
}

func newObservedCache[K comparable, V weightedCacheEntry](
	options *otter.Options[K, V],
	metrics observedCacheMetrics,
) (*observedCache[K, V], error) {
	c := &observedCache[K, V]{}
	c.metrics = metrics

	optionsCopy := *options
	options = &optionsCopy
	options.StatsRecorder = c

	var err error
	c.Cache, err = otter.New(options)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *observedCache[K, V]) Get(ctx context.Context, key K, loader otter.Loader[K, V]) (V, error) {
	observedLoader := trackedLoader[K, V]{loader: loader}
	val, err := c.Cache.Get(ctx, key, &observedLoader)
	if err == nil {
		if observedLoader.loaded {
			if c.metrics.MissNumberCounter != nil {
				c.metrics.MissNumberCounter.Inc()
			}
			if c.metrics.MissWeightCounter != nil {
				c.metrics.MissWeightCounter.Add(float64(val.Weight()))
			}
		} else {
			if c.metrics.HitNumberCounter != nil {
				c.metrics.HitNumberCounter.Inc()
			}
			if c.metrics.HitWeightCounter != nil {
				c.metrics.HitWeightCounter.Add(float64(val.Weight()))
			}
		}
	}
	return val, err
}

func (c *observedCache[K, V]) RecordHits(count int)   {}
func (c *observedCache[K, V]) RecordMisses(count int) {}
func (c *observedCache[K, V]) RecordEviction(weight uint32) {
	if c.metrics.EvictionNumberCounter != nil {
		c.metrics.EvictionNumberCounter.Inc()
	}
	if c.metrics.EvictionWeightCounter != nil {
		c.metrics.EvictionWeightCounter.Add(float64(weight))
	}
}
func (c *observedCache[K, V]) RecordLoadSuccess(loadTime time.Duration) {}
func (c *observedCache[K, V]) RecordLoadFailure(loadTime time.Duration) {}
