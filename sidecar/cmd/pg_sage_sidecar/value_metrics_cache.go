package main

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/value"
)

// valueMetricsTTL bounds how often /metrics reads every value ledger: the
// ledgers move on action and verification timescales, not per scrape.
const valueMetricsTTL = 60 * time.Second

// valueMetricsCache holds the last complete read of the value ledgers for
// valueMetricsTTL. The mutex is held across the read, so parallel scrapes
// share one in-flight read. A read with any failed database is not kept:
// the next scrape retries it instead of reporting a stale outage.
type valueMetricsCache struct {
	mu      sync.Mutex
	now     func() time.Time
	read    func(context.Context, []value.Source) ([]value.SourceResult, error)
	sources []value.Source
	at      time.Time
	results []value.SourceResult
}

var valueMetrics = newValueMetricsCache(time.Now, readValueLedgers)

func newValueMetricsCache(
	now func() time.Time,
	read func(context.Context, []value.Source) ([]value.SourceResult, error),
) *valueMetricsCache {
	return &valueMetricsCache{now: now, read: read}
}

func readValueLedgers(
	ctx context.Context, sources []value.Source,
) ([]value.SourceResult, error) {
	reader := value.NewFleetService(func() []value.Source { return sources })
	return reader.Read(ctx, value.Filter{})
}

// get returns the cached results for this exact set of sources while they
// are younger than the TTL, and reads the ledgers otherwise.
func (c *valueMetricsCache) get(
	ctx context.Context, sources []value.Source,
) ([]value.SourceResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.results != nil && slices.Equal(c.sources, sources) &&
		c.now().Sub(c.at) < valueMetricsTTL {
		return c.results, nil
	}
	results, err := c.read(ctx, sources)
	if err != nil || anyValueReadFailed(results) {
		c.results = nil
		return results, err
	}
	// The kept copy names the fleet the results belong to: a database
	// added, removed or reconnected (a new pool) is read at once.
	c.sources = slices.Clone(sources)
	c.at, c.results = c.now(), results
	return results, nil
}

func anyValueReadFailed(results []value.SourceResult) bool {
	for _, result := range results {
		if result.Err != nil {
			return true
		}
	}
	return false
}
