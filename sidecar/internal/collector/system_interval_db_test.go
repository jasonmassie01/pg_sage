package collector

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// collection-06 on real PostgreSQL: the system interval of two real
// snapshots, across pg_stat_reset().

func sysIntervalCollector(t *testing.T) (*pgxpool.Pool, *Collector) {
	t.Helper()
	return sysIntervalCollectorOn(t, connectPool(t, testdb.CreateDatabase(t, "system_interval")))
}

// sysIntervalCollectorOn is sysIntervalCollector on pool's database.
func sysIntervalCollectorOn(t *testing.T, pool *pgxpool.Pool) (*pgxpool.Pool, *Collector) {
	t.Helper()
	ctx := context.Background()
	var version int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).
		Scan(&version); err != nil {
		t.Fatalf("version: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE public.iv_t AS
		SELECT g AS id, repeat('x', 200) AS pad FROM generate_series(1, 5000) g`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	waitDatabaseStatsPublished(t, pool)
	return pool, New(pool, testConfig(), version, noopLog)
}

// waitDatabaseStatsPublished waits until pg_stat_database shows the
// fixture's own work in the freshly created database: PG14's statistics
// collector publishes a backend's counters up to 500 ms late, so a first
// snapshot taken at once could read zeros for a database nobody has
// reported on yet (round 3: "counters not read: hit=0 commit=0").
func waitDatabaseStatsPublished(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for range 40 {
		var ready bool
		if err := pool.QueryRow(ctx, `SELECT blks_hit > 0 AND xact_commit + xact_rollback > 0
			FROM pg_catalog.pg_stat_database WHERE datname = current_database()`).
			Scan(&ready); err != nil {
			t.Fatalf("read pg_stat_database: %v", err)
		}
		if ready {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("pg_stat_database never showed the fixture's work within 10 s")
}

// collectAndPublish runs one real collection and publishes it as latest,
// as cycle does.
func collectAndPublish(t *testing.T, c *Collector) *Snapshot {
	t.Helper()
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	c.mu.Lock()
	c.previous, c.latest = c.latest, snap
	c.mu.Unlock()
	return snap
}

func ivWorkload(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for i := 0; i < 5; i++ {
		if _, err := pool.Exec(context.Background(),
			`SELECT count(*) FROM public.iv_t`); err != nil {
			t.Fatalf("workload: %v", err)
		}
	}
	// Backends flush pending statistics at most once a second (PG15+; PG14's
	// collector every 500 ms), so the counters show the workload after that.
	time.Sleep(1500 * time.Millisecond)
}

// assertNonNegative fails on any negative interval value (CHECK-28).
func assertNonNegative(t *testing.T, iv *SystemInterval) {
	t.Helper()
	for name, p := range map[string]*int64{"blks_accessed": iv.BlksAccessed,
		"deadlocks": iv.Deadlocks, "xacts": iv.Xacts, "temp_bytes": iv.TempBytes,
		"checkpoints": iv.Checkpoints} {
		if p != nil && *p < 0 {
			t.Errorf("%s = %d, negative", name, *p)
		}
	}
	for name, p := range map[string]*float64{"cache_hit_ratio": iv.CacheHitRatio,
		"blk_read_time_ms": iv.BlkReadTimeMs, "blk_write_time_ms": iv.BlkWriteTimeMs} {
		if p != nil && *p < 0 {
			t.Errorf("%s = %v, negative", name, *p)
		}
	}
}

func TestSystemIntervalDB_FirstMeasuredAndCounters(t *testing.T) {
	pool, c := sysIntervalCollector(t)
	first := collectAndPublish(t, c)
	if first.System.Interval != nil {
		t.Fatalf("first snapshot interval = %+v, want nil", *first.System.Interval)
	}
	if first.System.BlksHit <= 0 || first.System.XactCommit+first.System.XactRollback <= 0 {
		t.Fatalf("counters not read: hit=%d commit=%d rollback=%d", first.System.BlksHit,
			first.System.XactCommit, first.System.XactRollback)
	}
	if first.System.CheckpointStatsEpoch.IsZero() {
		t.Fatal("checkpoint stats epoch not read")
	}
	ivWorkload(t, pool)
	second := collectAndPublish(t, c)
	iv := second.System.Interval
	if iv == nil || iv.BlksAccessed == nil || iv.Xacts == nil || iv.Checkpoints == nil {
		t.Fatalf("second interval not measured: %+v", iv)
	}
	if iv.Seconds <= 0 || *iv.BlksAccessed <= 0 || *iv.Xacts <= 0 {
		t.Fatalf("interval = %+v, want positive seconds, blocks and transactions", *iv)
	}
	if iv.CacheHitRatio == nil || *iv.CacheHitRatio > 1 {
		t.Fatalf("interval ratio = %v, want a fraction", iv.CacheHitRatio)
	}
	assertNonNegative(t, iv)
}

// After pg_stat_reset() the next interval's database fields are nil and
// the one after is measured again; nothing is ever negative.
func TestSystemIntervalDB_ResetThenMeasured(t *testing.T) {
	pool, c := sysIntervalCollector(t)
	ctx := context.Background()
	ivWorkload(t, pool)
	before := collectAndPublish(t, c)
	time.Sleep(10 * time.Millisecond)
	if _, err := pool.Exec(ctx, `SELECT pg_stat_reset()`); err != nil {
		t.Fatalf("pg_stat_reset: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	var reset *Snapshot
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond) // PG14 applies resets asynchronously
		reset = collectAndPublish(t, c)
		if reset.System.RelationStatsEpoch.After(before.System.RelationStatsEpoch) {
			break
		}
		if reset.System.Interval != nil {
			assertNonNegative(t, reset.System.Interval)
		}
	}
	if !reset.System.RelationStatsEpoch.After(before.System.RelationStatsEpoch) {
		t.Fatalf("relation stats epoch stayed %s", reset.System.RelationStatsEpoch)
	}
	if reset.System.Interval == nil {
		t.Fatal("reset interval = nil, want one with nil database fields")
	}
	assertDBFieldsNil(t, reset.System.Interval)
	assertNonNegative(t, reset.System.Interval)
	ivWorkload(t, pool)
	after := collectAndPublish(t, c)
	iv := after.System.Interval
	if iv == nil || iv.BlksAccessed == nil || iv.Deadlocks == nil || iv.Xacts == nil {
		t.Fatalf("interval after reset not measured: %+v", iv)
	}
	if *iv.Deadlocks != 0 || *iv.BlksAccessed <= 0 {
		t.Errorf("after reset: deadlocks %d blocks %d, want 0 and > 0",
			*iv.Deadlocks, *iv.BlksAccessed)
	}
	assertNonNegative(t, iv)
}
