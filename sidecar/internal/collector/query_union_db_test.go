package collector

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
)

// collection-07 on a real server: a fresh database per test (textTestDB),
// so its pg_stat_statements rows are this test's alone.

// runCompetitors runs n distinct statements calls times each, every call
// sleeping sleep seconds, concurrently: a high total time at a modest mean.
// The statements differ by their number of output columns, so each has
// its own queryid.
func runCompetitors(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	n, calls int, sleep string) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 1; i <= n; i++ {
		sql := "SELECT pg_sleep(" + sleep + ") AS competitor" + strings.Repeat(", 1", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range calls {
				if _, err := pool.Exec(ctx, sql); err != nil {
					errs <- fmt.Errorf("%s: %w", sql, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("competitor workload: %v", err)
	}
}

// queryIDOf is the queryid of this database's statement whose text holds
// marker.
func queryIDOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, marker string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `SELECT queryid FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND strpos(query, $1) > 0 LIMIT 1`, marker).Scan(&id)
	if err != nil {
		t.Fatalf("queryid of %q: %v", marker, err)
	}
	return id
}

func queryWith(qs []QueryStats, marker string) (QueryStats, bool) {
	for _, q := range qs {
		if strings.Contains(q.Query, marker) {
			return q, true
		}
	}
	return QueryStats{}, false
}

func unionCollector(t *testing.T, pool *pgxpool.Pool, n int) *Collector {
	t.Helper()
	cfg := testConfig()
	cfg.Collector.MaxQueries = n
	logs := func(level, msg string, args ...any) {
		if level == "WARN" || level == "ERROR" {
			t.Logf(level+" "+msg, args...)
		}
	}
	return New(pool, cfg, serverVersion(t, pool), logs)
}

func bootstrapSage(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap sage schema: %v", err)
	}
}

// storeRowsAt counts the query_store rows of queryid captured at at.
func storeRowsAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	queryid int64, at time.Time) int {
	t.Helper()
	var n int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.query_store
		WHERE queryid = $1 AND captured_at = $2`, queryid, at).Scan(&n)
	if err != nil {
		t.Fatalf("count query_store rows: %v", err)
	}
	return n
}

// recordCycle runs the queries step at the fake time at (a future time)
// and records its query_store samples, as a cycle does. query_store stamps
// captured_at with the server's now(), so the cycle's rows are moved to at
// afterwards: the cycles of a verification window run in seconds.
func recordCycle(t *testing.T, ctx context.Context, c *Collector, at time.Time) []QueryStats {
	t.Helper()
	var started time.Time
	if err := c.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&started); err != nil {
		t.Fatalf("server clock: %v", err)
	}
	c.now = func() time.Time { return at }
	qs := textCycle(t, ctx, c)
	if len(qs) > c.maxQueries() {
		t.Fatalf("%d queries published, over max_queries %d", len(qs), c.maxQueries())
	}
	snap := &Snapshot{CollectedAt: at, Queries: qs,
		StatsEpoch: c.collectStatementsEpoch(ctx)}
	c.recordQueryStore(ctx, snap)
	_, err := c.pool.Exec(ctx, `UPDATE sage.query_store SET captured_at = $1
		WHERE captured_at >= $2 AND captured_at <= clock_timestamp()`, at, started)
	if err != nil {
		t.Fatalf("move the cycle's samples to %s: %v", at, err)
	}
	return qs
}

// CHECK-01, happy path: 22 statements with more total time each than a
// two-call 200 ms report push it past the 2N - 2K = 18 rows of the total
// ranking (N = 10), as hundreds of OLTP statements do in production. It
// is still collected, admitted by its mean alone; the old read missed it.
func TestCollectQueries_SlowRareStatementAdmittedByMean(t *testing.T) {
	ctx := context.Background()
	pool := textTestDB(t, ctx)
	runCompetitors(t, ctx, pool, 22, 6, "0.08")
	for range 2 {
		mustExec(t, pool, "SELECT pg_sleep(0.2) AS rare_report_marker")
	}
	c := unionCollector(t, pool, 10)
	qs := textCycle(t, ctx, c)
	if len(qs) > 10 {
		t.Fatalf("%d queries published, want at most N = 10", len(qs))
	}
	q, ok := queryWith(qs, "rare_report_marker")
	if !ok {
		t.Fatalf("the slow two-call statement was not collected: %v", idsOf(qs))
	}
	if !slices.Equal(q.AdmittedBy, []string{"mean"}) || q.Calls != 2 || q.MeanExecTime < 190 {
		t.Fatalf("slow statement: admitted_by %v calls %d mean %.1f ms; want [mean], 2, >= 190",
			q.AdmittedBy, q.Calls, q.MeanExecTime)
	}
	legacy, err := legacyQueries(ctx, c)
	if err != nil {
		t.Fatalf("legacy read: %v", err)
	}
	if _, ok := queryWith(legacy, "rare_report_marker"); ok {
		t.Fatal("control: the top-N by total holds the slow statement; the test proves nothing")
	}
}

// State transition: a watched queryid outside the top-N gets query_store
// rows every cycle until its watch expires (fake clock), then none.
func TestCollectQueries_WatchedQueryRecordedUntilExpiry(t *testing.T) {
	ctx := context.Background()
	pool := textTestDB(t, ctx)
	bootstrapSage(t, ctx, pool)
	runCompetitors(t, ctx, pool, 22, 6, "0.08")
	const target = "SELECT pg_sleep(0.001) AS watched_target_marker"
	mustExec(t, pool, target)
	id := queryIDOf(t, ctx, pool, "watched_target_marker")
	c := unionCollector(t, pool, 10)
	start := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	steps := []struct {
		at     time.Duration
		pinned bool
	}{{time.Minute, false}, {5 * time.Minute, true}, {15 * time.Minute, true},
		{25 * time.Minute, false}, {35 * time.Minute, false}}
	for i, step := range steps {
		if i == 1 {
			c.Watch(id, start.Add(25*time.Minute))
		}
		for range 3 {
			mustExec(t, pool, target)
		}
		at := start.Add(step.at)
		q, in := queryWith(recordCycle(t, ctx, c, at), "watched_target_marker")
		rows := storeRowsAt(t, ctx, pool, id, at)
		want := 0
		if step.pinned {
			want = 1
		}
		if in != step.pinned || rows != want {
			t.Fatalf("+%s: collected %v, %d query_store rows; want %v and %d",
				step.at, in, rows, step.pinned, want)
		}
		if in && !slices.Equal(q.AdmittedBy, []string{"pinned"}) {
			t.Fatalf("+%s: admitted_by %v, want [pinned]", step.at, q.AdmittedBy)
		}
	}
}

// CHECK-03: pg_stat_statements(true) is read only for the admitted rows:
// one text read, for at most 2N queryids, while the database holds more;
// the ranking itself reads no text and runs once.
func TestCollectQueries_TextReadOnlyForAdmittedRows(t *testing.T) {
	ctx := context.Background()
	pool := textTestDB(t, ctx)
	runCompetitors(t, ctx, pool, 40, 2, "0.001")
	mustExec(t, pool, "SELECT pg_sleep(0.001) AS text_read_watch_marker")
	c := unionCollector(t, pool, 10)
	c.Watch(queryIDOf(t, ctx, pool, "text_read_watch_marker"), time.Now().Add(time.Hour))
	var mu sync.Mutex
	var texts [][]int64
	counters := 0
	c.onCatalogQuery = func(_ context.Context, _ pgx.Tx, sql string, args []any) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(sql, `pg_stat_statements"(true)`):
			ids, _ := args[0].([]int64)
			texts = append(texts, ids)
		case strings.Contains(sql, `pg_stat_statements"(false)`) &&
			strings.Contains(sql, "admitted_by"):
			counters++
		}
	}
	qs := textCycle(t, ctx, c)
	var entries int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT queryid) FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`).
		Scan(&entries); err != nil {
		t.Fatalf("count entries: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if counters != 1 || len(texts) != 1 {
		t.Fatalf("%d ranked reads and %d text reads, want 1 each", counters, len(texts))
	}
	if len(texts[0]) > 20 || entries <= 20 {
		t.Fatalf("text read for %d queryids of %d; want at most 2N = 20 of more",
			len(texts[0]), entries)
	}
	for _, q := range qs {
		if !slices.Contains(texts[0], q.QueryID) {
			t.Errorf("query %d published without being in the text read", q.QueryID)
		}
	}
}

// admissionOf runs the ranked read for N = 10 with own as the queryids
// known to be pg_sage's own, and returns how queryid was admitted.
func admissionOf(t *testing.T, ctx context.Context, c *Collector, queryid int64,
	own []int64) ([]string, bool) {
	t.Helper()
	caps := c.queryCapabilities(ctx)
	names, err := c.pgss()
	if err != nil {
		t.Fatalf("pg_stat_statements names: %v", err)
	}
	qs, err := readQueryCounters(ctx, timedCatalogQuerier{collector: c}, names.fn, caps,
		rankedRead{admission: admissionFor(10), own: own})
	if err != nil {
		t.Fatalf("ranked read: %v", err)
	}
	for _, q := range qs {
		if q.QueryID == queryid {
			return q.AdmittedBy, true
		}
	}
	return nil, false
}

// runSpilling runs sql twice, each in a transaction with a 64 kB work_mem,
// so its sort writes temp blocks.
func runSpilling(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) {
	t.Helper()
	for range 2 {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL work_mem = '64kB'"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, sql)
			return err
		})
		if err != nil {
			t.Fatalf("spilling statement: %v", err)
		}
	}
}

// The io arm admits a statement by its temp blocks written, and on a real
// server neither the mean nor the io arm admits a queryid the text cache
// knows as pg_sage's own ($2): the same read without own ids (the control)
// admits it by both, and the mean arm still takes its row, the next
// slowest statement.
func TestReadQueryCounters_OwnIDsLeftOutOfMeanAndIO(t *testing.T) {
	ctx := context.Background()
	pool := textTestDB(t, ctx)
	runSpilling(t, ctx, pool, `SELECT pg_sleep(0.3), count(*) AS heavy_own_marker
		FROM (SELECT g FROM generate_series(1, 300000) g ORDER BY g DESC) s`)
	for range 2 {
		mustExec(t, pool, "SELECT pg_sleep(0.1) AS second_slow_marker")
	}
	c := unionCollector(t, pool, 10)
	heavy := queryIDOf(t, ctx, pool, "heavy_own_marker")
	second := queryIDOf(t, ctx, pool, "second_slow_marker")
	if arms, ok := admissionOf(t, ctx, c, heavy, nil); !ok ||
		!slices.Contains(arms, "mean") || !slices.Contains(arms, "io") {
		t.Fatalf("control: heavy statement admitted %v (%v); want by mean and io", arms, ok)
	}
	if arms, _ := admissionOf(t, ctx, c, heavy, []int64{heavy}); slices.Contains(arms, "mean") ||
		slices.Contains(arms, "io") {
		t.Fatalf("own statement admitted by %v; want neither mean nor io", arms)
	}
	if arms, ok := admissionOf(t, ctx, c, second, []int64{heavy}); !ok ||
		!slices.Contains(arms, "mean") {
		t.Fatalf("next slowest statement admitted %v (%v); want by mean", arms, ok)
	}
}
