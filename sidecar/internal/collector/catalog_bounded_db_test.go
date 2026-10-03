package collector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// measured.md M3 / static.md F3 / M7: index definitions are cached and
// re-read only for changed indexes, the collector no longer opens every
// relation each cycle (relcache stays bounded), pg_database_size runs on a
// slow cadence, and every catalog transaction is bounded.

// defRecorder counts the index oids whose definitions were fetched.
type defRecorder struct{ oids []uint32 }

func (d *defRecorder) hook(_ context.Context, _ pgx.Tx, sql string, args []any) {
	if !strings.Contains(sql, "pg_get_indexdef(") || len(args) != 1 {
		return
	}
	if oids, ok := args[0].([]uint32); ok {
		d.oids = append(d.oids, oids...)
	}
}

func indexOID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) uint32 {
	t.Helper()
	var oid uint32
	if err := pool.QueryRow(ctx, "SELECT $1::regclass::oid", name).Scan(&oid); err != nil {
		t.Fatalf("oid of %s: %v", name, err)
	}
	return oid
}

func defOf(idx []IndexStats, oid uint32) string {
	for _, i := range idx {
		if i.IndexRelID == oid {
			return i.IndexDef
		}
	}
	return ""
}

func TestCollectIndexes_DefinitionsFetchedOnlyWhenChanged(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS defcache CASCADE")
	})
	if _, err := pool.Exec(ctx, `CREATE SCHEMA defcache;
		CREATE TABLE defcache.t (id int PRIMARY KEY, a int, b int);
		CREATE INDEX t_a ON defcache.t (a);
		CREATE TABLE defcache.u (id int PRIMARY KEY, c int);
		CREATE INDEX u_c ON defcache.u (c) WHERE c > 0`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	rec := &defRecorder{}
	c.onCatalogQuery = rec.hook
	first, err := c.collectIndexes(ctx)
	if err != nil || len(rec.oids) != len(first) {
		t.Fatalf("cycle 1 fetched %d defs for %d indexes (%v)", len(rec.oids), len(first), err)
	}
	rec.oids = nil
	second, err := c.collectIndexes(ctx)
	if err != nil || len(rec.oids) != 0 {
		t.Fatalf("cycle 2 fetched %d defs over an unchanged catalog (%v)", len(rec.oids), err)
	}
	if asJSON(t, first) != asJSON(t, second) {
		t.Fatal("cached cycle returned different index stats")
	}
	if _, err := pool.Exec(ctx, `REINDEX INDEX defcache.t_a;
		CREATE INDEX t_b ON defcache.t (b)`); err != nil {
		t.Fatalf("reindex/create: %v", err)
	}
	rec.oids = nil
	third, err := c.collectIndexes(ctx)
	if err != nil {
		t.Fatalf("cycle 3: %v", err)
	}
	assertRefetched(t, ctx, pool, rec.oids, "defcache.t_a", "defcache.t_b")
	if d := defOf(third, indexOID(t, ctx, pool, "defcache.t_b")); !strings.Contains(d, "(b)") {
		t.Fatalf("new index def = %q", d)
	}
	renameColumnIsSeen(t, ctx, pool, c)
}

// assertRefetched: both changed indexes were re-read, and nothing outside
// their table was.
func assertRefetched(t *testing.T, ctx context.Context, pool *pgxpool.Pool, got []uint32,
	names ...string) {
	t.Helper()
	seen := map[uint32]bool{}
	for _, o := range got {
		seen[o] = true
	}
	for _, n := range names {
		if !seen[indexOID(t, ctx, pool, n)] {
			t.Fatalf("changed index %s not re-read (re-read %v)", n, got)
		}
	}
	var outside int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM unnest($1::oid[]) o
		JOIN pg_index x ON x.indexrelid = o
		WHERE x.indrelid <> 'defcache.t'::regclass`, got).Scan(&outside); err != nil {
		t.Fatalf("check refetch set: %v", err)
	}
	if outside != 0 {
		t.Fatalf("%d indexes of unchanged tables re-read: %v", outside, got)
	}
}

// renameColumnIsSeen: a column rename touches neither pg_class row, only
// pg_attribute; the collector must still notice (within a few cycles, as
// statistics flush).
func renameColumnIsSeen(t *testing.T, ctx context.Context, pool *pgxpool.Pool, c *Collector) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := conn.Exec(ctx, "ALTER TABLE defcache.u RENAME COLUMN c TO cc"); err != nil {
		conn.Release()
		t.Fatalf("rename: %v", err)
	}
	flushStats(t, ctx, conn.Conn())
	conn.Release()
	oid := indexOID(t, ctx, pool, "defcache.u_c")
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := c.collectIndexes(ctx)
		if err != nil {
			t.Fatalf("collect after rename: %v", err)
		}
		if d := defOf(got, oid); strings.Contains(d, "(cc)") && strings.Contains(d, "cc > 0") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("renamed column never reached the cached def: %q", defOf(got, oid))
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func backendMemory(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (int32, int64) {
	t.Helper()
	var pid int32
	var bytes int64
	if err := pool.QueryRow(ctx, `SELECT pg_backend_pid(),
		(SELECT sum(total_bytes)::int8 FROM pg_backend_memory_contexts)`).
		Scan(&pid, &bytes); err != nil {
		t.Fatalf("backend memory: %v", err)
	}
	return pid, bytes
}

// static.md F3: one pass of the size functions grew CacheMemoryContext to
// 299 MB on lifeos. 1,200 tables x 3 indexes grew it ~20 MB under the old
// SQL; the rewrite opens only the exact top-N.
func TestCollectCatalog_BackendCatalogCacheStaysBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	createManySchemas(t, ctx, "relcache_many")
	pool := singleConnPool(t)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	c.cfg.Collector.BatchSize = 1000
	if _, err := c.collectSystem(ctx); err != nil { // warm the backend
		t.Fatalf("warm-up: %v", err)
	}
	pid, before := backendMemory(t, ctx, pool)
	for cycle := 0; cycle < 2; cycle++ {
		if _, err := c.collectTables(ctx); err != nil {
			t.Fatalf("tables: %v", err)
		}
		if _, err := c.collectIndexes(ctx); err != nil {
			t.Fatalf("indexes: %v", err)
		}
	}
	// The pool's backend may have been replaced (bulk definition fetches
	// run on a hijacked scratch connection that is then closed); whichever
	// backend the pool holds now must be about as small as a warm one.
	pid2, after := backendMemory(t, ctx, pool)
	if grew := after - before; grew > 6<<20 {
		t.Fatalf("pool backend %d holds %d MB more than warm backend %d after two "+
			"cycles of 1,200 tables, want < 6 MB", pid2, grew>>20, pid)
	}
}

type sqlCounter struct{ n map[string]int }

func (s *sqlCounter) hook(_ context.Context, _ pgx.Tx, sql string, _ []any) {
	for _, k := range []string{"pg_database_size("} {
		if strings.Contains(sql, k) {
			s.n[k]++
		}
	}
}

func TestCollectSystem_DatabaseSizeOnSlowCadence(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	clock := time.Unix(5_000_000, 0)
	c.now = func() time.Time { return clock }
	counter := &sqlCounter{n: map[string]int{}}
	c.onCatalogQuery = counter.hook
	var first int64
	for cycle := 0; cycle < 15; cycle++ {
		s, err := c.collectSystem(ctx)
		if err != nil || s.DBSizeBytes <= 0 || s.MaxConnections <= 0 {
			t.Fatalf("cycle %d: %+v (%v)", cycle, s, err)
		}
		if cycle == 0 {
			first = s.DBSizeBytes
		} else if s.DBSizeBytes != first {
			t.Fatalf("cycle %d size %d, want the cached %d", cycle, s.DBSizeBytes, first)
		}
		clock = clock.Add(time.Minute)
	}
	if got := counter.n["pg_database_size("]; got != 1 {
		t.Fatalf("15 one-minute cycles measured the database %d times, want 1", got)
	}
	clock = clock.Add(DBSizeRefreshInterval)
	if _, err := c.collectSystem(ctx); err != nil {
		t.Fatalf("refresh cycle: %v", err)
	}
	if got := counter.n["pg_database_size("]; got != 2 {
		t.Fatalf("after the refresh interval the size was measured %d times, want 2", got)
	}
}

// measured.md M7: the size walk timed out and failed the whole system
// step (the snapshot's spine). It now keeps the rest and the last size.
func TestCollectSystem_DatabaseSizeTimeoutKeepsSystemStats(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	rec := &logRecorder{}
	cfg := testConfig()
	cfg.Safety.QueryTimeoutMs = 300
	c := New(pool, cfg, serverVersion(t, pool), rec.log)
	clock := time.Unix(6_000_000, 0)
	c.now = func() time.Time { return clock }
	s, err := c.collectSystem(ctx)
	if err != nil || s.DBSizeBytes <= 0 {
		t.Fatalf("first cycle: %+v (%v)", s, err)
	}
	old := databaseSizeSQL
	databaseSizeSQL = sageTag + "SELECT pg_database_size(current_database()) FROM pg_sleep(2)"
	t.Cleanup(func() { databaseSizeSQL = old })
	clock = clock.Add(DBSizeRefreshInterval)
	s2, err := c.collectSystem(ctx)
	if err != nil {
		t.Fatalf("a size timeout failed the system step: %v", err)
	}
	if s2.MaxConnections <= 0 || s2.DBSizeBytes != s.DBSizeBytes {
		t.Fatalf("system after size timeout = %+v, want stats and the last size %d", s2,
			s.DBSizeBytes)
	}
	if !rec.has("WARN", "database size") {
		t.Fatalf("logs = %v, want a warning about the database size", rec.lines)
	}
}

func TestCollectSystem_UnknownSizeWhenNeverMeasured(t *testing.T) {
	pool := testPool(t)
	cfg := testConfig()
	cfg.Safety.QueryTimeoutMs = 300
	c := New(pool, cfg, serverVersion(t, pool), noopLog)
	old := databaseSizeSQL
	databaseSizeSQL = sageTag + "SELECT pg_database_size(current_database()) FROM pg_sleep(2)"
	t.Cleanup(func() { databaseSizeSQL = old })
	s, err := c.collectSystem(context.Background())
	if err != nil || s.DBSizeBytes != 0 || s.MaxConnections <= 0 {
		t.Fatalf("system = %+v (%v), want stats with an unknown (0) size", s, err)
	}
}

// Every catalog statement runs read-only under the configured statement
// and lock timeouts, without parallel workers or JIT.
func TestCatalogQuery_BoundedTransaction(t *testing.T) {
	pool := testPool(t)
	cfg := testConfig()
	cfg.Safety.QueryTimeoutMs = 750
	c := New(pool, cfg, serverVersion(t, pool), noopLog)
	var got []string
	var probeErr error
	c.onCatalogQuery = func(ctx context.Context, tx pgx.Tx, _ string, _ []any) {
		var st, lt, par, jit, ro string
		probeErr = tx.QueryRow(ctx, `SELECT current_setting('statement_timeout'),
			current_setting('lock_timeout'),
			current_setting('max_parallel_workers_per_gather'),
			current_setting('jit'), current_setting('transaction_read_only')`).
			Scan(&st, &lt, &par, &jit, &ro)
		got = []string{st, lt, par, jit, ro}
	}
	if _, err := c.collectLocks(context.Background()); err != nil || probeErr != nil {
		t.Fatalf("collectLocks: %v (probe %v)", err, probeErr)
	}
	want := []string{"750ms", "30s", "0", "off", "on"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("catalog transaction settings = %v, want %v", got, want)
	}
	cfg.Safety.QueryTimeoutMs = 50
	c.onCatalogQuery = nil
	rows, err := c.catalogQuery(context.Background(), sageTag+"SELECT pg_sleep(1)")
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "statement timeout") {
		t.Fatalf("slow catalog query error = %v, want a statement timeout", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("timeout came from the context, not the server")
	}
}
