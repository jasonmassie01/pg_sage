package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/value"
)

// fakeLedgerRead counts ledger reads and returns one snapshot per source,
// or the configured failure.
type fakeLedgerRead struct {
	calls   atomic.Int32
	fail    error
	failDB  string
	release chan struct{}
	started chan struct{}
}

func (f *fakeLedgerRead) read(
	_ context.Context, sources []value.Source,
) ([]value.SourceResult, error) {
	f.calls.Add(1)
	if f.started != nil {
		// Never block on the signal: a cache that lets a second read
		// through must fail on the read count, not hang the test.
		select {
		case f.started <- struct{}{}:
		default:
		}
		<-f.release
	}
	if f.fail != nil {
		return nil, f.fail
	}
	results := make([]value.SourceResult, 0, len(sources))
	for _, source := range sources {
		result := value.SourceResult{Name: source.Name, Snapshot: value.Snapshot{
			ByFeatureMinutes: map[string]float64{"vacuum_table": 5}}}
		if source.Name == f.failDB {
			result.Err = errors.New("connection refused")
		}
		results = append(results, result)
	}
	return results, nil
}

type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }

func cacheUnderTest(read *fakeLedgerRead) (*valueMetricsCache, *fakeClock) {
	clock := &fakeClock{at: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	return newValueMetricsCache(clock.now, read.read), clock
}

var cacheSources = []value.Source{{Name: "orders"}, {Name: "billing"}}

// Two scrapes within the TTL make one ledger read; a scrape at the TTL
// reads again (earned-09).
func TestValueMetricsCacheReadsOncePerTTL(t *testing.T) {
	read := &fakeLedgerRead{}
	cache, clock := cacheUnderTest(read)
	ctx := context.Background()
	for _, step := range []struct {
		advance time.Duration
		reads   int32
	}{{0, 1}, {0, 1}, {valueMetricsTTL - time.Nanosecond, 1}, {time.Nanosecond, 2}, {0, 2}} {
		clock.at = clock.at.Add(step.advance)
		results, err := cache.get(ctx, cacheSources)
		if err != nil || len(results) != 2 || results[0].Name != "orders" ||
			results[0].Snapshot.ByFeatureMinutes["vacuum_table"] != 5 {
			t.Fatalf("get = %+v, %v", results, err)
		}
		if got := read.calls.Load(); got != step.reads {
			t.Fatalf("after +%s: %d ledger reads, want %d", step.advance, got, step.reads)
		}
	}
}

// A changed fleet (a database added, or a new pool) is read at once.
func TestValueMetricsCacheRereadsWhenTheFleetChanges(t *testing.T) {
	read := &fakeLedgerRead{}
	cache, _ := cacheUnderTest(read)
	ctx := context.Background()
	if _, err := cache.get(ctx, cacheSources); err != nil {
		t.Fatal(err)
	}
	grown := append(append([]value.Source(nil), cacheSources...), value.Source{Name: "audit"})
	results, err := cache.get(ctx, grown)
	if err != nil || len(results) != 3 || read.calls.Load() != 2 {
		t.Fatalf("grown fleet = %d results, %d reads, %v; want 3 and 2",
			len(results), read.calls.Load(), err)
	}
	repooled := []value.Source{{Name: "orders", Pool: &pgxpool.Pool{}}, {Name: "billing"}}
	if _, err := cache.get(ctx, repooled); err != nil || read.calls.Load() != 3 {
		t.Fatalf("reconnected database: %d reads, %v; want 3", read.calls.Load(), err)
	}
}

// A failed read, whole or for one database, is not kept: the next scrape
// retries instead of reporting a stale outage for a minute.
func TestValueMetricsCacheDoesNotKeepFailures(t *testing.T) {
	ctx := context.Background()
	whole := &fakeLedgerRead{fail: errors.New("fleet unavailable")}
	cache, _ := cacheUnderTest(whole)
	for i := 0; i < 2; i++ {
		if _, err := cache.get(ctx, cacheSources); err == nil ||
			err.Error() != "fleet unavailable" {
			t.Fatalf("get %d error = %v, want the read error", i, err)
		}
	}
	if got := whole.calls.Load(); got != 2 {
		t.Fatalf("whole-read failures: %d reads, want 2", got)
	}

	partial := &fakeLedgerRead{failDB: "billing"}
	cache, _ = cacheUnderTest(partial)
	for i := 0; i < 2; i++ {
		results, err := cache.get(ctx, cacheSources)
		if err != nil || len(results) != 2 || results[1].Err == nil {
			t.Fatalf("get %d = %+v, %v; want billing's error", i, results, err)
		}
	}
	if got := partial.calls.Load(); got != 2 {
		t.Fatalf("one-database failures: %d reads, want 2", got)
	}
}

// Parallel scrapes share one in-flight read.
func TestValueMetricsCacheSharesTheInFlightRead(t *testing.T) {
	read := &fakeLedgerRead{started: make(chan struct{}, 1), release: make(chan struct{})}
	cache, _ := cacheUnderTest(read)
	ctx := context.Background()
	const scrapes = 8
	var wg sync.WaitGroup
	errs := make([]error, scrapes)
	counts := make([]int, scrapes)
	for i := 0; i < scrapes; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			results, err := cache.get(ctx, cacheSources)
			errs[slot], counts[slot] = err, len(results)
		}(i)
	}
	<-read.started
	close(read.release)
	wg.Wait()
	if got := read.calls.Load(); got != 1 {
		t.Fatalf("%d parallel scrapes made %d ledger reads, want 1", scrapes, got)
	}
	for i := range errs {
		if errs[i] != nil || counts[i] != 2 {
			t.Fatalf("scrape %d = %d results, %v", i, counts[i], errs[i])
		}
	}
}

// An empty fleet is a valid (cached) read with no series.
func TestValueMetricsCacheEmptyFleet(t *testing.T) {
	read := &fakeLedgerRead{}
	cache, _ := cacheUnderTest(read)
	for i := 0; i < 2; i++ {
		results, err := cache.get(context.Background(), nil)
		if err != nil || len(results) != 0 {
			t.Fatalf("empty fleet = %+v, %v", results, err)
		}
	}
	if got := read.calls.Load(); got != 1 {
		t.Fatalf("empty fleet: %d reads, want 1", got)
	}
}
