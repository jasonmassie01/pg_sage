package collector

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// bootstrappedPool opens dsn and bootstraps the sage schema there.
func bootstrappedPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

// snapshotRows lists exactly the categories persist writes, sorted, and
// leaves out an unavailable category (unknown, not empty).
func TestSnapshotRows_CategoriesSortedAndUnavailableSkipped(t *testing.T) {
	snap := &Snapshot{CollectedAt: time.Now(), Unavailable: map[string]string{
		"indexes": "statement timeout"}}
	rows, err := snapshotRows(snap)
	if err != nil {
		t.Fatalf("snapshotRows: %v", err)
	}
	var cats []string
	for _, r := range rows {
		cats = append(cats, r.Category)
		if len(r.Data) == 0 {
			t.Errorf("%s: empty document", r.Category)
		}
	}
	want := "config_data,foreign_keys,io,locks,partitions,queries,replication,sequences," +
		"system,tables"
	if got := strings.Join(cats, ","); got != want {
		t.Fatalf("categories = %s\nwant %s", got, want)
	}
}

// Integration: from the second cycle on, catalog categories are stored as
// deltas on the first cycle's keyframes and read back as collected;
// system stays a full row every cycle.
func TestPersist_CatalogCategoriesAreDeltaEncoded(t *testing.T) {
	ctx := context.Background()
	pool := bootstrappedPool(t, ctx, testdb.CreateDatabase(t, "collector_dedupe"))
	if _, err := pool.Exec(ctx, `CREATE TABLE public.dedupe_t (id int PRIMARY KEY, v int);
		CREATE INDEX dedupe_t_v ON public.dedupe_t (v)`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	c := New(pool, testConfig(), 170000, noopLog)
	snap, err := c.collect(ctx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	start := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 2; i++ {
		snap.CollectedAt = start.Add(time.Duration(i) * time.Minute)
		if err := c.persist(ctx, snap); err != nil {
			t.Fatalf("persist %d: %v", i, err)
		}
	}
	for cat, wantDelta := range map[string]bool{"indexes": true, "tables": true,
		"system": false} {
		var first, second int64
		var secondBase *int64
		if err := pool.QueryRow(ctx, `SELECT min(id), max(id),
			(SELECT base_id FROM sage.snapshots WHERE category = $1
			  ORDER BY collected_at DESC LIMIT 1)
			FROM sage.snapshots WHERE category = $1`, cat).
			Scan(&first, &second, &secondBase); err != nil {
			t.Fatalf("%s rows: %v", cat, err)
		}
		isDelta := secondBase != nil && *secondBase == first
		if isDelta != wantDelta {
			t.Errorf("%s: second row base %v (first id %d), want delta=%v", cat,
				secondBase, first, wantDelta)
		}
	}
	assertStoredAsCollected(t, ctx, pool, []*Snapshot{snap})
}

// assertStoredAsCollected checks every stored document of the snapshots
// reads back, through the accessor, equal to what was collected.
func assertStoredAsCollected(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	snaps []*Snapshot) {
	t.Helper()
	for _, s := range snaps {
		rows, err := snapshotRows(s)
		if err != nil {
			t.Fatalf("snapshotRows: %v", err)
		}
		for _, r := range rows {
			var same bool
			if err := pool.QueryRow(ctx, `SELECT `+snapstore.DataSQL("")+` = $3::jsonb
				FROM sage.snapshots WHERE collected_at = $1 AND category = $2`,
				s.CollectedAt, r.Category, string(r.Data)).Scan(&same); err != nil || !same {
				t.Fatalf("%s at %s reads back different (%v)", r.Category, s.CollectedAt, err)
			}
		}
	}
}

// fixture5000 creates 250 tables with 20 indexes each (5,000 indexes).
const fixture5000 = `
CREATE SCHEMA app;
DO $$ BEGIN
  FOR t IN 1..250 LOOP
    EXECUTE format('CREATE TABLE app.t%s (id bigint PRIMARY KEY, %s, created_at timestamptz)',
      t, (SELECT string_agg(format('c%s int', c), ', ') FROM generate_series(1, 19) c));
    FOR c IN 1..19 LOOP
      EXECUTE format('CREATE INDEX ix_t%s_c%s ON app.t%s (c%s, created_at DESC)', t, c, t, c);
    END LOOP;
  END LOOP;
END $$`

// workload scans a few indexes and inserts a few rows, so counters move
// between cycles the way a quiet application's do.
func workload(t *testing.T, ctx context.Context, pool *pgxpool.Pool, cycle int) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin workload: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("workload: %v", err)
	}
	for k := 0; k < 5; k++ {
		table := 1 + (cycle*7+k*31)%250
		col := 1 + (cycle+k)%19
		tbl := "app.t" + strconv.Itoa(table)
		sql := "SELECT count(*) FROM " + tbl + " WHERE c" + strconv.Itoa(col) + " = 1"
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatalf("workload scan: %v", err)
		}
		ins := "INSERT INTO " + tbl + " (id, c1) VALUES ($1, 1)"
		if _, err := tx.Exec(ctx, ins, cycle*10+k); err != nil {
			t.Fatalf("workload insert: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit workload: %v", err)
	}
}

// insertLegacy writes s the way persist did before the delta format: one
// full row per category. (The shared snapfixture helper imports this
// package, so it cannot be used from its own tests.)
func insertLegacy(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s *Snapshot) {
	t.Helper()
	rows, err := snapshotRows(s)
	if err != nil {
		t.Fatalf("snapshotRows: %v", err)
	}
	for _, r := range rows {
		if _, err := pool.Exec(ctx, `INSERT INTO sage.snapshots
			(collected_at, category, data) VALUES ($1, $2, $3)`,
			s.CollectedAt, r.Category, r.Data); err != nil {
			t.Fatalf("legacy insert %s: %v", r.Category, err)
		}
	}
}

// logCategoryBytes logs the stored jsonb bytes per category of both stores.
func logCategoryBytes(t *testing.T, ctx context.Context, legacy, pool *pgxpool.Pool) {
	t.Helper()
	const q = `SELECT category, sum(pg_column_size(data))::bigint FROM sage.snapshots
		GROUP BY category ORDER BY category`
	sizes := func(p *pgxpool.Pool) map[string]int64 {
		rows, err := p.Query(ctx, q)
		if err != nil {
			t.Fatalf("category bytes: %v", err)
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var cat string
			var n int64
			if err := rows.Scan(&cat, &n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[cat] = n
		}
		return out
	}
	old, cur := sizes(legacy), sizes(pool)
	for cat, n := range old {
		t.Logf("  %-13s legacy %9d B  delta %9d B", cat, n, cur[cat])
	}
}

func relationSize(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT pg_total_relation_size('sage.snapshots')`).Scan(&n); err != nil {
		t.Fatalf("relation size: %v", err)
	}
	return n
}

// One hour of real collection (60 cycles) on a database with 5,000 real
// indexes: the delta store writes at least 10x fewer bytes into
// sage.snapshots than the legacy format, and every document reads back as
// collected.
func TestPersist_BytesPerHourReal5000Indexes(t *testing.T) {
	ctx := context.Background()
	pool := bootstrappedPool(t, ctx, testdb.CreateDatabase(t, "collector_5000ix"))
	legacy := bootstrappedPool(t, ctx, testdb.CreateDatabase(t, "collector_5000ix_old"))
	if _, err := pool.Exec(ctx, fixture5000); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	cfg := testConfig()
	cfg.Collector.BatchSize = 1000
	c := New(pool, cfg, 170000, noopLog)
	start := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute)
	var snaps []*Snapshot
	for i := 0; i < 60; i++ {
		workload(t, ctx, pool, i)
		snap, err := c.collect(ctx)
		if err != nil {
			t.Fatalf("collect %d: %v", i, err)
		}
		if len(snap.Indexes) < 5000 {
			t.Fatalf("collected %d indexes, want >= 5000", len(snap.Indexes))
		}
		snap.CollectedAt = start.Add(time.Duration(i) * time.Minute)
		snaps = append(snaps, snap)
	}
	beforeNew, beforeOld := relationSize(t, ctx, pool), relationSize(t, ctx, legacy)
	for _, s := range snaps {
		if err := c.persist(ctx, s); err != nil {
			t.Fatalf("persist: %v", err)
		}
		insertLegacy(t, ctx, legacy, s)
	}
	newBytes := relationSize(t, ctx, pool) - beforeNew
	oldBytes := relationSize(t, ctx, legacy) - beforeOld
	t.Logf("sage.snapshots for one hour, 5,000 real indexes: legacy %d B, delta %d B (%.1fx)",
		oldBytes, newBytes, float64(oldBytes)/float64(max(newBytes, 1)))
	logCategoryBytes(t, ctx, legacy, pool)
	if newBytes*10 > oldBytes {
		t.Errorf("delta store wrote %d B against legacy %d B, want >= 10x less", newBytes,
			oldBytes)
	}
	assertStoredAsCollected(t, ctx, pool, snaps)
}
