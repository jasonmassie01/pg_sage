package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestIOMonitor(t *testing.T, pool *pgxpool.Pool) (*IOMonitor, *testClock) {
	t.Helper()
	name := fmt.Sprintf("iotest_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.io_rate_sample WHERE database_name=$1", name)
	})
	clock := &testClock{now: time.Now().UTC()}
	monitor := NewIOMonitor(pool, name, 14*24*time.Hour)
	monitor.now = clock.Now
	return monitor, clock
}

func serverVersion(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var version int
	if err := pool.QueryRow(t.Context(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	return version
}

func countIOSamples(t *testing.T, pool *pgxpool.Pool, database string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM sage.io_rate_sample
		WHERE database_name=$1`, database).Scan(&count); err != nil {
		t.Fatalf("count samples: %v", err)
	}
	return count
}

func TestReadIOCountersMatchesServerVersion(t *testing.T) {
	pool := verifyIntegrationPool(t)
	version := serverVersion(t, pool)
	counters, err := ReadIOCounters(t.Context(), pool)
	if err != nil {
		t.Fatalf("ReadIOCounters on %d: %v", version, err)
	}
	want := IOSourcePGStatDatabase
	if version >= 160000 {
		want = IOSourcePGStatIO
	}
	if counters.Source != want || len(counters.DataParts) == 0 {
		t.Fatalf("PG %d counters source=%q parts=%d, want %q with parts",
			version, counters.Source, len(counters.DataParts), want)
	}
	if counters.WAL.Bytes <= 0 {
		t.Fatalf("PG %d wal_bytes = %v, want cumulative WAL", version, counters.WAL.Bytes)
	}
	for key, part := range counters.DataParts {
		if part.Bytes < 0 {
			t.Fatalf("part %q has negative bytes %v", key, part.Bytes)
		}
	}
}

// generateIO writes and reads a private table so data and WAL counters move.
func generateIO(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	ctx := t.Context()
	statements := []string{
		"CREATE TABLE " + table + " (id bigint, payload text)",
		"INSERT INTO " + table + " SELECT g, repeat('x', 200) FROM generate_series(1, 60000) g",
		"SELECT count(*) FROM " + table,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("generate IO (%s): %v", statement, err)
		}
	}
}

func TestIOMonitorMeasuresRealDataAndWALRates(t *testing.T) {
	pool := verifyIntegrationPool(t)
	monitor, clock := newTestIOMonitor(t, pool)
	base := fmt.Sprintf("io_monitor_load_%d", time.Now().UnixNano())
	loads := 0
	load := func() {
		table := fmt.Sprintf("%s_%d", base, loads)
		loads++
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
		generateIO(t, pool, table)
	}
	if err := monitor.Sample(t.Context()); err != nil {
		t.Fatalf("prime sample: %v", err)
	}
	load()
	sawData, sawWAL := sampleUntilMeasured(t, monitor, clock, load)
	if !sawData || !sawWAL {
		t.Fatalf("real load never measured: data=%v wal=%v", sawData, sawWAL)
	}
	evidence, err := monitor.IOEvidence(t.Context())
	if err != nil || evidence.Baseline.Samples < 1 || evidence.Baseline.ObservedDays <= 0 {
		t.Fatalf("baseline from persisted samples = %+v, %v", evidence.Baseline, err)
	}
	if countIOSamples(t, pool, monitor.database) != evidence.Baseline.Samples {
		t.Fatalf("baseline does not reflect persisted samples")
	}
}

func TestIOMonitorConcurrentSamplersPersistOneRatePerInterval(t *testing.T) {
	pool := verifyIntegrationPool(t)
	monitor, clock := newTestIOMonitor(t, pool)
	if err := monitor.Sample(t.Context()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	clock.Advance(time.Minute)
	var workers sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := monitor.Sample(t.Context()); err != nil &&
				!errors.Is(err, ErrSampleTooSoon) {
				errs <- err
			}
		}()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent sample: %v", err)
	}
	if got := countIOSamples(t, pool, monitor.database); got != 1 {
		t.Fatalf("concurrent samplers persisted %d rates for one interval, want 1", got)
	}
	// A second sidecar sampling the same database adds its own rows, but the
	// overlapping minute is observed once.
	second := NewIOMonitor(pool, monitor.database, 14*24*time.Hour)
	second.now = clock.Now
	if err := second.Sample(t.Context()); err != nil {
		t.Fatalf("second prime: %v", err)
	}
	clock.Advance(time.Minute)
	for _, m := range []*IOMonitor{monitor, second} {
		if err := m.Sample(t.Context()); err != nil {
			t.Fatalf("sample: %v", err)
		}
	}
	evidence, err := monitor.IOEvidence(t.Context())
	if err != nil || evidence.Baseline.Samples != 3 {
		t.Fatalf("baseline = %+v, %v; want 3 samples", evidence.Baseline, err)
	}
	if want := 2.0 / (24 * 60); evidence.Baseline.ObservedDays > want+1e-12 {
		t.Fatalf("overlapping samplers double-counted: %v days > %v",
			evidence.Baseline.ObservedDays, want)
	}
}

func TestIOMonitorCounterResetIsNotLowLoad(t *testing.T) {
	pool := verifyIntegrationPool(t)
	version := serverVersion(t, pool)
	monitor, clock := newTestIOMonitor(t, pool)
	if err := monitor.Sample(t.Context()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	before := databaseStatsReset(t, pool)
	// pg_stat_reset() touches only this fixture database's counters.
	if _, err := pool.Exec(t.Context(), "SELECT pg_stat_reset()"); err != nil {
		t.Fatalf("pg_stat_reset: %v", err)
	}
	clock.Advance(time.Minute)
	err := sampleAfterStatsSettle(t, monitor, before)
	if version >= 160000 {
		// pg_stat_io and pg_stat_wal are shared; a database reset leaves
		// them comparable, so the interval is still valid evidence.
		if err != nil || countIOSamples(t, pool, monitor.database) != 1 {
			t.Fatalf("PG %d shared counters after database reset: %v", version, err)
		}
		return
	}
	if !errors.Is(err, ErrCounterReset) {
		t.Fatalf("PG %d reset sample error = %v, want ErrCounterReset", version, err)
	}
	if got := countIOSamples(t, pool, monitor.database); got != 0 {
		t.Fatalf("reset interval persisted %d samples", got)
	}
	evidence, evidenceErr := monitor.IOEvidence(t.Context())
	if evidenceErr != nil || evidence.Rate != nil ||
		!strings.Contains(evidence.RateError, "reset") {
		t.Fatalf("reset reported as evidence %+v, %v", evidence, evidenceErr)
	}
}

func databaseStatsReset(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var reset string
	if err := pool.QueryRow(t.Context(), `SELECT COALESCE(stats_reset::text, '')
		FROM pg_stat_database WHERE datname = current_database()`).Scan(&reset); err != nil {
		t.Fatalf("read stats_reset: %v", err)
	}
	return reset
}

// sampleAfterStatsSettle waits until the reset is visible (PG14/15 publish
// it through the asynchronous stats collector) before sampling.
func sampleAfterStatsSettle(t *testing.T, monitor *IOMonitor, before string) error {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for databaseStatsReset(t, monitor.pool) == before && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if databaseStatsReset(t, monitor.pool) == before {
		t.Fatal("pg_stat_reset() never became visible")
	}
	return monitor.Sample(t.Context())
}

func TestIOMonitorPrunesOnlyItsExpiredSamples(t *testing.T) {
	pool := verifyIntegrationPool(t)
	monitor, clock := newTestIOMonitor(t, pool)
	other := monitor.database + "_other"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.io_rate_sample WHERE database_name=$1", other)
	})
	old := clock.Now().Add(-15 * 24 * time.Hour)
	for _, name := range []string{monitor.database, other} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO sage.io_rate_sample
			(database_name, sampled_at, interval_seconds, data_bytes_per_sec,
			 wal_bytes_per_sec, source) VALUES ($1, $2, 60, 1, 1, $3)`,
			name, old, IOSourcePGStatIO); err != nil {
			t.Fatalf("seed old sample: %v", err)
		}
	}
	if err := monitor.Sample(t.Context()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	clock.Advance(time.Minute)
	if err := monitor.Sample(t.Context()); err != nil {
		t.Fatalf("sample: %v", err)
	}
	var total, expired int
	if err := pool.QueryRow(t.Context(), `SELECT count(*),
		count(*) FILTER (WHERE sampled_at < $2) FROM sage.io_rate_sample
		WHERE database_name=$1`, monitor.database, old.Add(time.Hour)).
		Scan(&total, &expired); err != nil {
		t.Fatalf("read own samples: %v", err)
	}
	if total != 1 || expired != 0 {
		t.Fatalf("own samples after prune = %d (%d expired), want only the new one",
			total, expired)
	}
	if got := countIOSamples(t, pool, other); got != 1 {
		t.Fatalf("prune removed another database's samples: %d", got)
	}
}

func TestIOMonitorStaleRateIsUnavailable(t *testing.T) {
	pool := verifyIntegrationPool(t)
	monitor, clock := newTestIOMonitor(t, pool)
	for range 2 {
		if err := monitor.Sample(t.Context()); err != nil {
			t.Fatalf("sample: %v", err)
		}
		clock.Advance(time.Minute)
	}
	fresh, err := monitor.IOEvidence(t.Context())
	if err != nil || fresh.Rate == nil {
		t.Fatalf("fresh evidence = %+v, %v", fresh, err)
	}
	clock.Advance(10 * time.Minute)
	stale, err := monitor.IOEvidence(t.Context())
	if err != nil || stale.Rate != nil || !strings.Contains(stale.RateError, "stale") {
		t.Fatalf("stale evidence = %+v, %v", stale, err)
	}
}

func TestIOMonitorWithoutPoolFailsClosed(t *testing.T) {
	monitor := NewIOMonitor(nil, "db", time.Hour)
	if err := monitor.Sample(t.Context()); err == nil {
		t.Fatal("sample without a pool succeeded")
	}
	if _, err := monitor.IOEvidence(t.Context()); err == nil {
		t.Fatal("evidence without a pool succeeded")
	}
	var nilMonitor *IOMonitor
	if _, err := nilMonitor.IOEvidence(t.Context()); err == nil {
		t.Fatal("nil monitor produced evidence")
	}
}

// sampleUntilMeasured samples until both data and WAL rates were seen.
// IO statistics are server-wide: another test package may reset them
// mid-interval, which the monitor correctly reports as ErrCounterReset.
// That interval proves nothing, so the load is generated again (at most
// 3 times) instead of failing the test.
func sampleUntilMeasured(t *testing.T, monitor *IOMonitor, clock *testClock,
	load func()) (bool, bool) {
	t.Helper()
	var sawData, sawWAL bool
	resets := 0
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !(sawData && sawWAL) {
		time.Sleep(500 * time.Millisecond)
		clock.Advance(10 * time.Second)
		err := monitor.Sample(t.Context())
		if errors.Is(err, ErrCounterReset) && resets < 3 {
			resets++
			t.Logf("IO statistics were reset by another session (%d); loading again", resets)
			load()
			continue
		}
		if err != nil {
			t.Fatalf("sample: %v", err)
		}
		evidence, err := monitor.IOEvidence(t.Context())
		if err != nil || evidence.Rate == nil {
			t.Fatalf("evidence = %+v, %v", evidence, err)
		}
		sawData = sawData || evidence.Rate.DataBytesPerSec > 0
		sawWAL = sawWAL || evidence.Rate.WALBytesPerSec > 0
	}
	return sawData, sawWAL
}
