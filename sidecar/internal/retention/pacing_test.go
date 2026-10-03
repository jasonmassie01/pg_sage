package retention

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// Perf storage phase: the cleaner used to delete 1,000 rows per statement
// back to back, as many statements as the backlog needed, inside the
// orchestrator's cycle. Batches are now bounded per table (TOAST-heavy
// snapshots in 50s), paced, and a run stops at its time budget and resumes
// on the next run.

// captureLog records log lines so tests can assert on them.
type captureLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *captureLog) log(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *captureLog) contains(level, part string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.HasPrefix(s, level+" ") && strings.Contains(s, part) {
			return true
		}
	}
	return false
}

func TestRunOnce_BatchesArePerTableAndPaced(t *testing.T) {
	pool, ctx := requireDB(t)
	tag := uniqueTag("paced")
	execRetry(t, ctx, `INSERT INTO sage.notification_log (event, subject, sent_at)
		SELECT $1, 's', now() - interval '400 days' FROM generate_series(1, 2500)`, tag)
	execRetry(t, ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		SELECT now() - interval '400 days', $1, '{}'::jsonb FROM generate_series(1, 120)`, tag)
	// A retained row in the same (history) partition: the expired rows are
	// deleted in batches; a wholly expired history partition would be
	// truncated instead (TestRunOnce_ExpiredHistoryIsTruncated).
	execRetry(t, ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES (now() - interval '1 hour', $1 || '_kept', '{}'::jsonb)`, tag)
	const pause = 30 * time.Millisecond
	c := New(pool, allDays(30), noopLog).WithPacing(pause, time.Minute)
	stats := c.RunOnce(ctx)
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.notification_log WHERE event=$1`,
		tag); n != 0 {
		t.Fatalf("%d expired notification_log rows remain", n)
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.snapshots WHERE category=$1`,
		tag); n != 0 {
		t.Fatalf("%d expired snapshots remain", n)
	}
	if got := stats.Batches["snapshots"]; got < 3 {
		t.Fatalf("snapshots purged in %d batches, want >= 3 of at most %d rows", got,
			snapshotBatchSize)
	}
	if got := stats.Batches["notification_log"]; got < 3 {
		t.Fatalf("notification_log purged in %d batches, want >= 3 of %d rows", got, batchSize)
	}
	if stats.Deleted["notification_log"] < 2500 || stats.Deleted["snapshots"] < 120 {
		t.Fatalf("deleted = %v", stats.Deleted)
	}
	// Every full batch is followed by a pause before the next one.
	full := stats.Batches["snapshots"] + stats.Batches["notification_log"] - 2
	if min := time.Duration(full) * pause; stats.Elapsed < min {
		t.Fatalf("run took %s, want at least %s of pauses", stats.Elapsed, min)
	}
	if len(stats.Deferred) != 0 {
		t.Fatalf("deferred = %v with a one-minute budget", stats.Deferred)
	}
}

func TestRunOnce_StopsAtItsBudgetAndResumesNextRun(t *testing.T) {
	pool, ctx := requireDB(t)
	tag := uniqueTag("budget")
	execRetry(t, ctx, `INSERT INTO sage.notification_log (event, subject, sent_at)
		SELECT $1, 's', now() - interval '400 days' FROM generate_series(1, 3500)`, tag)
	logs := &captureLog{}
	c := New(pool, allDays(30), logs.log).WithPacing(time.Millisecond, time.Nanosecond)
	start := time.Now()
	first := c.RunOnce(ctx)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("a run with a 1 ns budget took %s", time.Since(start))
	}
	if len(first.Deferred) == 0 || !first.BudgetSpent {
		t.Fatalf("first run = %+v, want the budget spent and rules deferred", first)
	}
	left := countWhere(t, ctx, `SELECT count(*) FROM sage.notification_log WHERE event=$1`, tag)
	if left == 0 {
		t.Fatal("a run over budget still deleted the whole backlog")
	}
	if !logs.contains("INFO", "budget") {
		t.Fatalf("no log line says the budget ran out: %v", logs.lines)
	}
	// Later runs continue where the last one stopped until nothing is left.
	for run := 0; run < 200 && left > 0; run++ {
		c.RunOnce(ctx)
		left = countWhere(t, ctx, `SELECT count(*) FROM sage.notification_log WHERE event=$1`,
			tag)
	}
	if left != 0 {
		t.Fatalf("%d rows left after 200 budget-bound runs", left)
	}
}

// A run cancelled by its context stops between batches and says so.
func TestRunOnce_CancelledContextStops(t *testing.T) {
	pool, _ := requireDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats := New(pool, allDays(30), noopLog).RunOnce(ctx)
	if stats.Statements != 0 {
		t.Fatalf("cancelled run issued %d statements", stats.Statements)
	}
}

func TestWithPacing_RejectsNonPositiveValues(t *testing.T) {
	c := New(nil, allDays(30), noopLog).WithPacing(-time.Second, 0)
	if c.pause != defaultPause || c.budget != defaultRunBudget {
		t.Fatalf("pacing = %s / %s, want the defaults", c.pause, c.budget)
	}
	if defaultRunBudget <= 0 || defaultRunBudget > time.Minute || defaultPause <= 0 {
		t.Fatalf("defaults: budget %s, pause %s", defaultRunBudget, defaultPause)
	}
	if snapshotBatchSize <= 0 || snapshotBatchSize > 100 {
		t.Fatalf("snapshotBatchSize = %d", snapshotBatchSize)
	}
}

// query_store has its own window: readers look back at most 7 days, so 90
// days of samples (14 GB projected on lifeos) were never read.
func TestRunOnce_QueryStoreUsesItsOwnWindow(t *testing.T) {
	pool, ctx := requireDB(t)
	const lo, hi = int64(8_400_000_001), int64(8_400_000_002)
	execRetry(t, ctx, `DELETE FROM sage.query_store WHERE queryid IN ($1, $2)`, lo, hi)
	execRetry(t, ctx, `INSERT INTO sage.query_store (captured_at, queryid, calls,
		total_exec_time, mean_exec_time) VALUES
		(now() - interval '20 days', $1, 1, 1, 1), (now() - interval '10 days', $2, 1, 1, 1)`,
		lo, hi)
	execRetry(t, ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES (now() - interval '20 days', 'qs_window_probe', '{}')`)
	cfg := &config.Config{Retention: config.RetentionConfig{SnapshotsDays: 90,
		QueryStoreDays: 14}}
	New(pool, cfg, noopLog).RunOnce(ctx)
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.query_store WHERE queryid=$1`,
		lo); n != 0 {
		t.Fatal("a 20-day-old query_store sample survived a 14-day window")
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.query_store WHERE queryid=$1`,
		hi); n != 1 {
		t.Fatal("a 10-day-old query_store sample was purged")
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.snapshots
		WHERE category='qs_window_probe'`); n != 1 {
		t.Fatal("snapshots followed the query_store window")
	}
	execRetry(t, ctx, `DELETE FROM sage.snapshots WHERE category='qs_window_probe'`)
	execRetry(t, ctx, `DELETE FROM sage.query_store WHERE queryid IN ($1, $2)`, lo, hi)
}

// explain_results is a cache: a row is useless once it expires. It used to
// age on created_at, which every cache refresh resets, so an expired row
// stayed up to explains_days and a hot one could never be purged.
func TestRunOnce_ExplainResultsAgeOnExpiry(t *testing.T) {
	pool, ctx := requireDB(t)
	tag := uniqueTag("explain")
	execRetry(t, ctx, `INSERT INTO sage.explain_results (query_hash, database_name,
		created_at, expires_at, plan_json, explanation) VALUES
		(1, $1, now() - interval '400 days', now() + interval '1 hour', '{}', '{}'),
		(2, $1, now() - interval '1 hour', now() - interval '3 days', '{}', '{}'),
		(3, $1, now() - interval '1 hour', now() - interval '1 hour', '{}', '{}')`, tag)
	New(pool, allDays(90), noopLog).RunOnce(ctx)
	rows, err := pool.Query(ctx, `SELECT query_hash FROM sage.explain_results
		WHERE database_name = $1 ORDER BY 1`, tag)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kept []int64
	for rows.Next() {
		var h int64
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, h)
	}
	// 1 is live however old; 2 expired days ago; 3 expired within the
	// one-day grace a reader racing the expiry may still need.
	if len(kept) != 2 || kept[0] != 1 || kept[1] != 3 {
		t.Fatalf("kept = %v, want [1 3]", kept)
	}
	execRetry(t, ctx, `DELETE FROM sage.explain_results WHERE database_name = $1`, tag)
}

// Resolved findings age from when they were resolved: last_seen is the
// time the analyzer last saw the problem, not when it went away.
func TestRunOnce_ResolvedFindingsAgeOnResolvedAt(t *testing.T) {
	pool, ctx := requireDB(t)
	tag := uniqueTag("resolved")
	execRetry(t, ctx, `INSERT INTO sage.findings (category, severity, object_type,
		object_identifier, title, detail, status, last_seen, resolved_at) VALUES
		($1, 'info', 'table', 'a', 't', '{}', 'resolved', now() - interval '300 days',
		 now() - interval '1 day'),
		($1, 'info', 'table', 'b', 't', '{}', 'resolved', now() - interval '300 days',
		 now() - interval '200 days'),
		($1, 'info', 'table', 'c', 't', '{}', 'open', now() - interval '300 days', NULL)`, tag)
	New(pool, allDays(180), noopLog).RunOnce(ctx)
	rows, err := pool.Query(ctx, `SELECT object_identifier FROM sage.findings
		WHERE category = $1 ORDER BY 1`, tag)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kept []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, s)
	}
	if strings.Join(kept, ",") != "a,c" {
		t.Fatalf("kept = %v, want a (resolved yesterday) and c (open)", kept)
	}
	execRetry(t, ctx, `DELETE FROM sage.findings WHERE category = $1`, tag)
}

// A rule for a table that does not exist yet (agent_db_* tables are
// created on first use) is skipped quietly, not logged as a failure.
func TestPurge_MissingOptionalTableIsSkipped(t *testing.T) {
	pool, ctx := requireDB(t)
	logs := &captureLog{}
	c := New(pool, allDays(30), logs.log)
	stats := newRunStats()
	c.purge(ctx, purgeRule{table: "agent_db_not_created", timeCol: "created_at", days: 1,
		optional: true}, &stats, time.Now().Add(time.Minute))
	if stats.Statements != 0 || logs.contains("ERROR", "agent_db_not_created") {
		t.Fatalf("optional missing table: stats %+v, logs %v", stats, logs.lines)
	}
	c.purge(ctx, purgeRule{table: "required_not_created", timeCol: "created_at", days: 1},
		&stats, time.Now().Add(time.Minute))
	if !logs.contains("ERROR", "required_not_created") {
		t.Fatalf("a missing required table was not logged: %v", logs.lines)
	}
}
