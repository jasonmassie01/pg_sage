package collector

import (
	"context"
	"math"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// otherDatabaseConn opens a session on the server's maintenance database
// (not the fixture) so tests can prove the collector ignores foreign-DB
// sessions. It only takes a transaction-scoped advisory lock there.
func otherDatabaseConn(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	u.Path = "/postgres"
	conn, err := pgx.Connect(context.Background(), u.String())
	if err != nil {
		t.Fatalf("connect maintenance database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// G1-B17: locks and idle-in-transaction counts must be scoped to the
// current database; foreign sessions (another tenant DB on the cluster)
// must not appear in this database's snapshot.
func TestCollectLocksAndActivity_ScopedToCurrentDatabase(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	other := otherDatabaseConn(t)
	var otherPID int
	if err := other.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&otherPID); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(ctx, `BEGIN`); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(ctx, `SELECT pg_advisory_xact_lock(917171)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = other.Exec(context.Background(), `ROLLBACK`) })

	c := New(pool, testConfig(), 170000, noopLog)
	locks, err := c.collectLocks(ctx)
	if err != nil {
		t.Fatalf("collectLocks: %v", err)
	}
	for _, lk := range locks {
		if lk.PID == otherPID {
			t.Fatalf("lock %s/%s from pid %d in another database leaked into snapshot",
				lk.LockType, lk.Mode, otherPID)
		}
	}
	assertLocalIdleInTransaction(t, ctx, pool, c)
}

// assertLocalIdleInTransaction checks the snapshot's idle-in-transaction
// count against this database's, read just before and just after it: a
// session of this database changing state in between would make two
// separate reads disagree, so such a window is measured again.
func assertLocalIdleInTransaction(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	c *Collector) {
	t.Helper()
	local := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE state = 'idle in transaction' AND datname = current_database()
			  AND backend_type = 'client backend'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for range 5 {
		before := local()
		s, err := c.collectSystem(ctx)
		if err != nil {
			t.Fatalf("collectSystem: %v", err)
		}
		if after := local(); after != before {
			continue
		}
		if s.IdleInTransaction != before {
			t.Fatalf("idle_in_transaction = %d, want %d (current database only)",
				s.IdleInTransaction, before)
		}
		return
	}
	t.Fatal("this database's idle-in-transaction sessions kept changing over 5 reads")
}

// G1-B18: a pagination error must not leave the keyset cursor behind;
// the next collection has to start from the beginning again. (The page
// used to fail on a locked table because the size functions locked every
// relation; the rewrite reads no relation, so the error is injected by
// canceling the context after the first page.)
func TestCollectTables_ErrorMidPaginationDoesNotSkipTables(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	mustExec(t, pool, `CREATE SCHEMA IF NOT EXISTS b18`)
	mustExec(t, pool, `CREATE TABLE IF NOT EXISTS b18.a_first (id int)`)
	mustExec(t, pool, `CREATE TABLE IF NOT EXISTS b18.b_locked (id int)`)

	cfg := testConfig()
	cfg.Collector.BatchSize = 1
	c := New(pool, cfg, 170000, noopLog)
	pageCtx, cancel := context.WithCancel(ctx)
	pages := 0
	c.onCatalogQuery = func(context.Context, pgx.Tx, string, []any) {
		if pages++; pages == 1 {
			cancel()
		}
	}
	if _, err := c.collectTables(pageCtx); err == nil {
		t.Fatal("expected an error while paging after the context was canceled")
	}
	c.onCatalogQuery = nil
	tables, err := c.collectTables(ctx)
	if err != nil {
		t.Fatalf("collectTables after recovery: %v", err)
	}
	found := map[string]bool{}
	for _, tb := range tables {
		if tb.SchemaName == "b18" {
			found[tb.RelName] = true
		}
	}
	if !found["a_first"] || !found["b_locked"] {
		t.Fatalf("collection after an error skipped tables: got %v", found)
	}
}

// A table held ACCESS EXCLUSIVE (VACUUM FULL, a rewrite) no longer stalls
// or fails the tables page: it is listed with estimated sizes, and only
// the best-effort exact sizing gives up (with a warning).
func TestCollectTables_LockedTableDoesNotBlockThePage(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	mustExec(t, pool, `CREATE SCHEMA IF NOT EXISTS b18`)
	mustExec(t, pool, `CREATE TABLE IF NOT EXISTS b18.b_locked (id int)`)
	locker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locker.Rollback(ctx) }()
	if _, err := locker.Exec(ctx, `LOCK TABLE b18.b_locked IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Safety.QueryTimeoutMs = 2000
	cfg.Safety.LockTimeoutMs = 100
	rec := &logRecorder{}
	c := New(pool, cfg, 170000, rec.log)
	c.exactTopN = 1 << 30
	tables, err := c.collectTables(ctx)
	if err != nil {
		t.Fatalf("collectTables with a locked table: %v", err)
	}
	listed := false
	for _, tb := range tables {
		listed = listed || (tb.SchemaName == "b18" && tb.RelName == "b_locked")
	}
	if !listed {
		t.Fatal("locked table missing from the tables page")
	}
	if !rec.has("WARN", "exact") {
		t.Fatalf("logs = %v, want a warning that exact sizes were skipped", rec.lines)
	}
}

// G1-B20: NULL LSNs (walsender in startup/catchup) must scan into nil
// instead of failing the whole replication category.
func TestScanReplicaRows_NullLSNs(t *testing.T) {
	pool := testPool(t)
	rows, err := pool.Query(context.Background(), `SELECT
		'10.0.0.9'::text, 'startup'::text,
		NULL::text, NULL::text, NULL::text, NULL::text,
		NULL::text, NULL::text, NULL::text, 'async'::text`)
	if err != nil {
		t.Fatal(err)
	}
	replicas, err := scanReplicaRows(rows)
	if err != nil {
		t.Fatalf("NULL LSN row rejected: %v", err)
	}
	if len(replicas) != 1 {
		t.Fatalf("got %d replicas, want 1", len(replicas))
	}
	r := replicas[0]
	if r.SentLSN != nil || r.ReplayLSN != nil || r.State != "startup" {
		t.Fatalf("replica = %+v, want nil LSNs and state startup", r)
	}
}

// C18 / G1-B35: pct_used follows the direction of travel over the
// configured [min, max] range; sage-owned sequences are excluded.
func TestCollectSequences_DirectionAndRange(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	stmts := []string{
		`CREATE SCHEMA IF NOT EXISTS b35`,
		`CREATE SCHEMA IF NOT EXISTS sage`,
		`DROP SEQUENCE IF EXISTS b35.asc_seq, b35.desc_seq, b35.shift_seq, sage.b35_owned`,
		`CREATE SEQUENCE b35.asc_seq MINVALUE 1 MAXVALUE 101`,
		`SELECT setval('b35.asc_seq', 76)`,
		`CREATE SEQUENCE b35.desc_seq INCREMENT -1 MINVALUE -101 MAXVALUE -1 START -1`,
		`SELECT setval('b35.desc_seq', -76)`,
		`CREATE SEQUENCE b35.shift_seq MINVALUE 1000 MAXVALUE 1100 START 1000 CYCLE`,
		`SELECT setval('b35.shift_seq', 1090)`,
		`CREATE SEQUENCE sage.b35_owned`,
	}
	for _, s := range stmts {
		mustExec(t, pool, s)
	}
	seqs, err := New(pool, testConfig(), 170000, noopLog).collectSequences(ctx)
	if err != nil {
		t.Fatalf("collectSequences: %v", err)
	}
	got := map[string]SequenceStats{}
	for _, s := range seqs {
		if s.SchemaName == "sage" {
			t.Fatalf("sage-owned sequence %s collected", s.SequenceName)
		}
		if s.SchemaName == "b35" {
			got[s.SequenceName] = s
		}
	}
	want := map[string]float64{"asc_seq": 75, "desc_seq": 75, "shift_seq": 90}
	for name, pct := range want {
		s, ok := got[name]
		if !ok {
			t.Fatalf("sequence %s missing", name)
		}
		if math.Abs(s.PctUsed-pct) > 0.01 {
			t.Errorf("%s pct_used = %v, want %v", name, s.PctUsed, pct)
		}
	}
	if got["desc_seq"].MinValue != -101 || got["shift_seq"].MinValue != 1000 {
		t.Errorf("min_value not collected: desc=%d shift=%d",
			got["desc_seq"].MinValue, got["shift_seq"].MinValue)
	}
	if !got["shift_seq"].Cycle || got["asc_seq"].Cycle {
		t.Errorf("cycle flags wrong: shift=%v asc=%v",
			got["shift_seq"].Cycle, got["asc_seq"].Cycle)
	}
}

func lsnPtr(s string) *string { return &s }
