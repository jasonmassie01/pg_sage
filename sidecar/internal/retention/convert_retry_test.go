package retention

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/partition"
)

// A deployment upgraded with a large or busy sage.query_store keeps the
// plain table when the conversion cannot finish (bootstrap leaves large
// tables to retention; a lock or statement timeout aborts it). pg_sage
// keeps running on it, retention still bounds it with paced deletes, and
// the conversion is retried with backoff, not on every cycle.

func TestConversionBackoffDoublesToADay(t *testing.T) {
	var b backoff
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	if !b.due("query_store", t0) {
		t.Fatal("a table never tried is not due")
	}
	want := []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour,
		16 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	at := t0
	for i, w := range want {
		if got := b.failed("query_store", at); got != w {
			t.Fatalf("failure %d: next attempt in %s, want %s", i+1, got, w)
		}
		if b.due("query_store", at.Add(w-time.Minute)) {
			t.Fatalf("failure %d: due before its backoff", i+1)
		}
		if !b.due("query_store", at.Add(w)) {
			t.Fatalf("failure %d: not due after its backoff", i+1)
		}
		if !b.due("snapshots", at) {
			t.Fatal("one table's failures delayed another")
		}
		at = at.Add(w)
	}
	b.succeeded("query_store")
	if !b.due("query_store", at) {
		t.Fatal("success did not reset the backoff")
	}
	if got := b.failed("query_store", at); got != time.Hour {
		t.Fatalf("first failure after a success: %s, want 1h", got)
	}
}

// plainQueryStore replaces sage.query_store with the plain table of a
// pg_sage version before partitioning, as an upgrade finds it.
func plainQueryStore(t *testing.T, ctx context.Context) {
	t.Helper()
	execRetry(t, ctx, `DROP TABLE IF EXISTS sage.query_store CASCADE;
		CREATE TABLE sage.query_store (
		    id              bigserial PRIMARY KEY,
		    captured_at     timestamptz NOT NULL DEFAULT now(),
		    queryid         bigint NOT NULL,
		    calls           bigint NOT NULL,
		    total_exec_time double precision NOT NULL,
		    mean_exec_time  double precision NOT NULL,
		    rows            bigint NOT NULL DEFAULT 0,
		    plan_hash       text,
		    stats_epoch     timestamptz);
		CREATE INDEX idx_query_store_qid_time ON sage.query_store (queryid, captured_at DESC);
		CREATE INDEX idx_query_store_time ON sage.query_store (captured_at DESC)`)
	t.Cleanup(func() {
		// Leave the partitioned layout the rest of the package expects.
		if _, err := partition.Convert(context.Background(), testPool,
			partition.QueryStore); err != nil {
			t.Logf("restore query_store layout: %v", err)
		}
	})
}

// logRecorder collects log lines by level.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) log(level, msg string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, level+" "+fmt.Sprintf(msg, args...))
}

func (r *logRecorder) matching(level, substr string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, l := range r.lines {
		if strings.HasPrefix(l, level+" ") && strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return out
}

// lockHolder keeps ACCESS SHARE on rel from its own connection (the test
// pool has one connection, used by the cleaner).
func lockHolder(t *testing.T, ctx context.Context, rel string) func() {
	t.Helper()
	conn, err := pgx.Connect(ctx, testDSN())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM "+rel+" LIMIT 1"); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = tx.Rollback(context.Background())
			_ = conn.Close(context.Background())
		})
	}
	t.Cleanup(release)
	return release
}

func outcomeFor(t *testing.T, outs []ConvertOutcome, table string) ConvertOutcome {
	t.Helper()
	for _, o := range outs {
		if o.Table == table {
			return o
		}
	}
	t.Fatalf("no outcome for %s in %+v", table, outs)
	return ConvertOutcome{}
}

func TestConvertHistory_FailsSoftRetriesWithBackoffThenSucceeds(t *testing.T) {
	pool, ctx := requireDB(t)
	plainQueryStore(t, ctx)
	execRetry(t, ctx, `INSERT INTO sage.query_store (captured_at, queryid, calls,
		total_exec_time, mean_exec_time)
		SELECT now() - interval '40 days' + g * interval '1 second', 8600000000, g, 1, 1
		FROM generate_series(1, 2500) g`)
	execRetry(t, ctx, `INSERT INTO sage.query_store (captured_at, queryid, calls,
		total_exec_time, mean_exec_time) VALUES (now(), 8600000001, 1, 1, 1)`)
	logs := &logRecorder{}
	cfg := config.DefaultConfig()
	c := New(pool, cfg, logs.log).WithPacing(time.Millisecond, time.Minute)

	release := lockHolder(t, ctx, "sage.query_store")
	now := time.Now()
	out := outcomeFor(t, c.ConvertHistory(ctx, now), "query_store")
	if !out.Attempted || out.Err == nil {
		t.Fatalf("outcome = %+v, want a failed attempt", out)
	}
	warns := logs.matching("WARN", "sage.query_store")
	if len(warns) != 1 || !strings.Contains(warns[0], "lock") ||
		!strings.Contains(warns[0], "1h0m0s") || !strings.Contains(warns[0], "keeps") {
		t.Fatalf("WARN lines = %q, want one naming the lock and the 1h retry", warns)
	}
	if k := relkindOf(t, ctx, "query_store"); k != "r" {
		t.Fatalf("relkind = %q, want the plain table", k)
	}
	// Not due again within the backoff: no attempt, no new WARN.
	out = outcomeFor(t, c.ConvertHistory(ctx, now.Add(30*time.Minute)), "query_store")
	if out.Attempted || len(logs.matching("WARN", "sage.query_store")) != 1 {
		t.Fatalf("retried inside the backoff: %+v", out)
	}
	release()

	// Retention still bounds the plain table: paced batch deletes.
	stats := c.RunOnce(ctx)
	if got := stats.Deleted["query_store"]; got != 2500 {
		t.Fatalf("deleted %d expired query_store rows from the plain table, want 2500", got)
	}
	if got := stats.Batches["query_store"]; got < 3 {
		t.Fatalf("%d batches for 2500 rows, want paced batches of %d", got, batchSize)
	}

	out = outcomeFor(t, c.ConvertHistory(ctx, now.Add(time.Hour)), "query_store")
	if !out.Attempted || out.Err != nil || !out.Result.Converted {
		t.Fatalf("retry after the backoff = %+v", out)
	}
	if k := relkindOf(t, ctx, "query_store"); k != "p" {
		t.Fatalf("relkind = %q after the retry, want partitioned", k)
	}
	if infos := logs.matching("INFO", "sage.query_store"); len(infos) == 0 ||
		!strings.Contains(infos[len(infos)-1], "lock held") {
		t.Fatalf("INFO lines = %q, want the conversion reported with its lock hold", infos)
	}
	// Partitioned now: nothing more to try.
	out = outcomeFor(t, c.ConvertHistory(ctx, now.Add(48*time.Hour)), "query_store")
	if out.Attempted {
		t.Fatalf("attempted to convert a partitioned table: %+v", out)
	}
}

// The orchestrator calls Run on its cycle; a conversion (VALIDATE of a
// large table can take minutes) must not hold the cycle.
func TestRun_DoesNotWaitForTheConversion(t *testing.T) {
	_, ctx := requireDB(t)
	plainQueryStore(t, ctx)
	// A production-sized pool: the shared test pool has one connection,
	// which the background attempt would hold.
	pool, err := pgxpool.New(ctx, testDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	logs := &logRecorder{}
	// No query_store purge: its DELETE would queue behind the attempt's queued
	// lock request (bounded by the lock timeout, but this test is about the
	// conversion, not the lock queue).
	cfg := config.DefaultConfig()
	cfg.Retention.QueryStoreDays = 0
	c := New(pool, cfg, logs.log)
	release := lockHolder(t, ctx, "sage.query_store")
	start := time.Now()
	c.Run(ctx)
	ran := time.Since(start)
	c.waitConversions()
	release()
	if len(logs.matching("WARN", "sage.query_store")) != 1 {
		t.Fatalf("background attempt did not report its failure: %q", logs.lines)
	}
	// The attempt waited out the lock timeout in the background; the
	// retention run itself did not.
	if ran >= partition.LockTimeout {
		t.Fatalf("Run took %s: it waited for the conversion", ran)
	}
}

// A conversion that died mid-way left its CHECK; the next retention run
// removes it whatever the backoff says (once the clock passes the cutover
// it would refuse every insert).
func TestConvertHistory_RemovesLeftoverCheckEveryRun(t *testing.T) {
	pool, ctx := requireDB(t)
	plainQueryStore(t, ctx)
	c := New(pool, config.DefaultConfig(), noopLog)
	release := lockHolder(t, ctx, "sage.query_store")
	now := time.Now()
	c.ConvertHistory(ctx, now) // fails: backoff starts
	release()
	execRetry(t, ctx, `ALTER TABLE sage.query_store ADD CONSTRAINT query_store_cutover_check
		CHECK (captured_at IS NOT NULL AND captured_at < now() + interval '1 day') NOT VALID`)
	out := outcomeFor(t, c.ConvertHistory(ctx, now.Add(time.Minute)), "query_store")
	if out.Attempted {
		t.Fatalf("converted inside the backoff: %+v", out)
	}
	var n int
	queryRetry(t, ctx, `SELECT count(*) FROM pg_constraint
		WHERE conname = 'query_store_cutover_check'`, &n)
	if n != 0 {
		t.Fatal("leftover cutover CHECK not removed")
	}
}

func relkindOf(t *testing.T, ctx context.Context, table string) string {
	t.Helper()
	var k string
	if err := testPool.QueryRow(ctx, `SELECT relkind::text FROM pg_class
		WHERE oid = to_regclass($1)`, "sage."+table).Scan(&k); err != nil {
		t.Fatalf("relkind of %s: %v", table, err)
	}
	return k
}
