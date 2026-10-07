package retention

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/partition"
)

// rebound simulates a deployment that has run for daysBack days on the
// partitioned layout: the history partition ends daysBack days ago (it is
// emptied: test data, and recreated if retention dropped it), and every day
// since has its own partition. A negative daysBack is a deployment that
// converted today: the history partition still covers today.
func rebound(t *testing.T, ctx context.Context, tbl partition.Table, daysBack int) time.Time {
	t.Helper()
	bound := partition.DayStart(time.Now()).AddDate(0, 0, -daysBack)
	reboundAt(t, ctx, tbl, bound, daysBack+3)
	return bound
}

// reboundAt bounds the (emptied) history partition at bound and ensures
// days daily partitions from it.
func reboundAt(t *testing.T, ctx context.Context, tbl partition.Table, bound time.Time,
	days int) {
	t.Helper()
	parts, err := partition.List(ctx, testPool, tbl)
	if err != nil {
		t.Fatalf("list %s: %v", tbl.Name, err)
	}
	for _, p := range parts {
		if !p.History && !p.Default {
			if err := partition.Drop(ctx, testPool, tbl, p); err != nil {
				t.Fatalf("drop %s: %v", p.Name, err)
			}
		}
	}
	// Looked up before the transaction: the test pool has one connection.
	detach := fmt.Sprintf("ALTER TABLE sage.%s DETACH PARTITION sage.%s", tbl.Name,
		tbl.HistoryName())
	if !relationExists(t, ctx, "sage."+tbl.HistoryName()) {
		detach = fmt.Sprintf(`CREATE TABLE sage.%s (LIKE sage.%s INCLUDING DEFAULTS
			INCLUDING CONSTRAINTS INCLUDING STORAGE)`, tbl.HistoryName(), tbl.Name)
	}
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, stmt := range []string{
		fmt.Sprintf("TRUNCATE sage.%s", tbl.Name),
		detach,
		fmt.Sprintf("ALTER TABLE sage.%s ATTACH PARTITION sage.%s FOR VALUES FROM (MINVALUE) "+
			"TO ('%s')", tbl.Name, tbl.HistoryName(), bound.Format(time.RFC3339)),
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := partition.Ensure(ctx, testPool, tbl, bound, max(days, 1)); err != nil {
		t.Fatalf("ensure %s: %v", tbl.Name, err)
	}
}

func relationExists(t *testing.T, ctx context.Context, name string) bool {
	t.Helper()
	var ok bool
	if err := testPool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).
		Scan(&ok); err != nil {
		t.Fatalf("look up %s: %v", name, err)
	}
	return ok
}

func partitionNames(t *testing.T, ctx context.Context, tbl partition.Table) []string {
	t.Helper()
	parts, err := partition.List(ctx, testPool, tbl)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		names = append(names, p.Name)
	}
	return names
}

// With daily partitions, query_store retention drops whole days: no row
// is deleted, nothing is left for vacuum, and the newest expired day goes
// as soon as all of it is older than the window.
func TestRunOnce_DropsExpiredQueryStoreDays(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := partition.QueryStore
	bound := rebound(t, ctx, tbl, 20)
	t.Cleanup(func() { rebound(t, ctx, tbl, 3) })
	for d := 0; d <= 20; d++ {
		execRetry(t, ctx, `INSERT INTO sage.query_store (captured_at, queryid, calls,
			total_exec_time, mean_exec_time) VALUES ($1, 8500000000 + $2, 1, 1, 1)`,
			bound.AddDate(0, 0, d).Add(12*time.Hour), d)
	}
	cfg := &config.Config{Retention: config.RetentionConfig{QueryStoreDays: 14}}
	stats := New(pool, cfg, noopLog).RunOnce(ctx)
	// The history partition ended 20 days ago and is empty: it goes too.
	want := []string{tbl.HistoryName()}
	for d := 0; d < 6; d++ { // days -20 .. -15 end at or before now - 14 days
		want = append(want, tbl.DayName(bound.AddDate(0, 0, d)))
	}
	slices.Sort(want)
	slices.Sort(stats.Dropped)
	if !slices.Equal(stats.Dropped, want) {
		t.Fatalf("dropped = %v, want %v", stats.Dropped, want)
	}
	if stats.Deleted["query_store"] != 0 {
		t.Fatalf("deleted %d query_store rows; expired days must be dropped, not deleted",
			stats.Deleted["query_store"])
	}
	names := partitionNames(t, ctx, tbl)
	for _, w := range want {
		if slices.Contains(names, w) {
			t.Fatalf("%s still exists: %v", w, names)
		}
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.query_store
		WHERE queryid BETWEEN 8500000000 AND 8500000020`); n != 15 {
		t.Fatalf("%d rows remain, want the 15 newest days'", n)
	}
	// Nothing left to drop: a second run is a no-op.
	if again := New(pool, cfg, noopLog).RunOnce(ctx); len(again.Dropped) != 0 {
		t.Fatalf("second run dropped %v", again.Dropped)
	}
}

// insertBlob writes a snapshot row of roughly kb kilobytes that does not
// compress (hex of random hashes) on day at.
func insertBlob(t *testing.T, ctx context.Context, at time.Time, kb int) int64 {
	t.Helper()
	return insertID(t, ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		SELECT $1, 'cap_test', jsonb_build_array(string_agg(md5(random()::text), ''))
		FROM generate_series(1, $2::int * 32) RETURNING id`, at, kb)
}

func partitionBytes(t *testing.T, ctx context.Context, name string) int64 {
	t.Helper()
	var n int64
	queryRetry(t, ctx, fmt.Sprintf(`SELECT pg_total_relation_size('sage.%s')`, name), &n)
	return n
}

// The size cap removes the oldest data first: the history partition goes
// (dropped whole when all of it must go and it covers no current time),
// then whole days are dropped, oldest first, until the table fits. Today's
// partition is never removed.
func TestRunOnce_SnapshotCapRemovesOldestDaysFirst(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := partition.Snapshots
	bound := rebound(t, ctx, tbl, 6)
	insertBlob(t, ctx, bound.Add(-time.Hour), 200) // history
	for d := 0; d < 6; d++ {
		for i := 0; i < 2; i++ {
			insertBlob(t, ctx, bound.AddDate(0, 0, d).Add(time.Duration(i+1)*time.Hour), 200)
		}
	}
	insertBlob(t, ctx, time.Now(), 200) // today
	total, err := partition.Size(ctx, pool, tbl)
	if err != nil {
		t.Fatal(err)
	}
	// Room for everything but the history rows and the two oldest days.
	freed := partitionBytes(t, ctx, tbl.DayName(bound)) +
		partitionBytes(t, ctx, tbl.DayName(bound.AddDate(0, 0, 1)))
	c := New(pool, &config.Config{Retention: config.RetentionConfig{SnapshotsDays: 90}},
		noopLog)
	c.capBytes = total - freed - 8192
	t.Cleanup(func() { rebound(t, ctx, tbl, 3) })
	stats := c.RunOnce(ctx)
	slices.Sort(stats.Dropped)
	want := []string{tbl.HistoryName(), tbl.DayName(bound), tbl.DayName(bound.AddDate(0, 0, 1))}
	slices.Sort(want)
	if !slices.Equal(stats.Dropped, want) {
		t.Fatalf("dropped = %v, want the history partition and the two oldest days %v",
			stats.Dropped, want)
	}
	if stats.Deleted["snapshots"] != 0 {
		t.Fatalf("deleted %d rows one by one; the history partition must be dropped whole",
			stats.Deleted["snapshots"])
	}
	after, err := partition.Size(ctx, pool, tbl)
	if err != nil || after > c.capBytes {
		t.Fatalf("size after = %d (cap %d), %v", after, c.capBytes, err)
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.snapshots
		WHERE category = 'cap_test' AND collected_at > now() - interval '1 hour'`); n != 1 {
		t.Fatal("today's snapshot was removed")
	}
	execRetry(t, ctx, `DELETE FROM sage.snapshots WHERE category = 'cap_test'`)
}

// A day whose keyframe a retained delta in a later day still needs is not
// removed, by age or by size; it goes once nothing retained needs it.
func TestRunOnce_SnapshotDayNeededByARetainedDeltaIsKept(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := partition.Snapshots
	bound := rebound(t, ctx, tbl, 4)
	keyframe := insertBlob(t, ctx, bound.Add(time.Hour), 100)
	delta := insertID(t, ctx, `INSERT INTO sage.snapshots (collected_at, category, data,
		base_id) VALUES ($1, 'cap_test', '{"n": 1}', $2) RETURNING id`,
		time.Now().Add(-time.Hour), keyframe)
	logs := &captureLog{}
	cfg := &config.Config{Retention: config.RetentionConfig{SnapshotsDays: 2}}
	c := New(pool, cfg, logs.log)
	c.capBytes = 1 // over the cap as well
	stats := c.RunOnce(ctx)
	if slices.Contains(stats.Dropped, tbl.DayName(bound)) {
		t.Fatalf("dropped %s while delta %d needs its keyframe", tbl.DayName(bound), delta)
	}
	if !logs.contains("WARN", tbl.DayName(bound)) {
		t.Fatalf("keeping %s was not logged: %v", tbl.DayName(bound), logs.lines)
	}
	assertNoOrphans(t)
	execRetry(t, ctx, `DELETE FROM sage.snapshots WHERE id = $1`, delta)
	stats = c.RunOnce(ctx)
	if !slices.Contains(stats.Dropped, tbl.DayName(bound)) {
		t.Fatalf("dropped = %v after the delta went, want %s", stats.Dropped,
			tbl.DayName(bound))
	}
	// Over a 1-byte cap everything older than today goes, never today.
	today := tbl.DayName(time.Now())
	if slices.Contains(stats.Dropped, today) ||
		!slices.Contains(partitionNames(t, ctx, tbl), today) {
		t.Fatalf("today's partition %s was removed for the cap: %v", today, stats.Dropped)
	}
	execRetry(t, ctx, `DELETE FROM sage.snapshots WHERE category = 'cap_test'`)
}

// Without a cap override the cap is retention.snapshots_max_pct of the
// database, never below the floor; 0 disables it.
func TestSnapshotCap_FromConfig(t *testing.T) {
	c := New(nil, &config.Config{Retention: config.RetentionConfig{SnapshotsMaxPct: 5}},
		noopLog)
	if got := c.snapshotCap(100 << 30); got != 5<<30 {
		t.Fatalf("cap of a 100 GiB database = %d, want 5 GiB", got)
	}
	if got := c.snapshotCap(1 << 30); got != config.MinSnapshotCapBytes {
		t.Fatalf("cap of a 1 GiB database = %d, want the floor %d", got, config.MinSnapshotCapBytes)
	}
	off := New(nil, &config.Config{}, noopLog)
	if got := off.snapshotCap(100 << 30); got != 0 {
		t.Fatalf("disabled cap = %d", got)
	}
}

// Once every row of the history partition (pre-upgrade data) is past the
// window it is dropped: no batch of deletes, no dead tuples, and its disk
// space is returned at once (a TRUNCATE would leave an empty partition
// behind for good).
func TestRunOnce_ExpiredHistoryIsDropped(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := partition.Snapshots
	bound := rebound(t, ctx, tbl, 3)
	t.Cleanup(func() { rebound(t, ctx, tbl, 3) })
	execRetry(t, ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		SELECT now() - interval '40 days', 'history_test', '{}'::jsonb
		FROM generate_series(1, 300)`)
	cfg := &config.Config{Retention: config.RetentionConfig{SnapshotsDays: 30}}
	stats := New(pool, cfg, noopLog).RunOnce(ctx)
	if !slices.Contains(stats.Dropped, tbl.HistoryName()) {
		t.Fatalf("dropped = %v, want the history partition", stats.Dropped)
	}
	if relationExists(t, ctx, "sage."+tbl.HistoryName()) {
		t.Fatal("the expired history partition still exists")
	}
	if stats.Deleted["snapshots"] != 0 {
		t.Fatalf("deleted %d snapshot rows one by one", stats.Deleted["snapshots"])
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.snapshots
		WHERE category = 'history_test'`); n != 0 {
		t.Fatalf("%d expired history rows remain", n)
	}
	// A history partition still holding a retained row is purged row by row.
	bound = rebound(t, ctx, tbl, 3)
	insertBlob(t, ctx, bound.Add(-time.Hour), 1)
	execRetry(t, ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES (now() - interval '40 days', 'history_test', '{}')`)
	stats = New(pool, cfg, noopLog).RunOnce(ctx)
	if slices.Contains(stats.Dropped, tbl.HistoryName()) || stats.Deleted["snapshots"] != 1 {
		t.Fatalf("stats = %+v, want one row deleted and the history partition kept", stats)
	}
	execRetry(t, ctx, `DELETE FROM sage.snapshots WHERE category = 'cap_test'`)
}

// The history partition can be mostly expired at once (an upgrade with a
// shorter window). Its purge must walk the time index, not scan the heap:
// a LIMIT over a sequential scan reads all of it once nothing is left.
func TestPurgeSQL_HistoryPartitionUsesItsTimeIndex(t *testing.T) {
	_, ctx := requireDB(t)
	tbl := partition.QueryStore
	rebound(t, ctx, tbl, 2)
	execRetry(t, ctx, `INSERT INTO sage.query_store (captured_at, queryid, calls,
		total_exec_time, mean_exec_time)
		SELECT now() - interval '30 days' - g * interval '1 second', g, 1, 1, 1
		FROM generate_series(1, 20000) g`)
	execRetry(t, ctx, `ANALYZE sage.query_store_history`)
	rule := purgeRules(&config.Config{Retention: config.RetentionConfig{
		QueryStoreDays: 14}})[1]
	if rule.table != "query_store" {
		t.Fatalf("rule = %+v", rule)
	}
	var plan string
	queryRetry(t, ctx, fmt.Sprintf("EXPLAIN (FORMAT JSON) %s",
		strings.Replace(purgeSQL(rule, "sage."+tbl.HistoryName(), batchSize), "$1", "14", 1)),
		&plan)
	if strings.Contains(plan, `"Seq Scan"`) || !strings.Contains(plan, `"Tid Scan"`) ||
		!strings.Contains(plan, `"Index Scan"`) {
		t.Fatalf("history purge plan scans the heap:\n%s", plan)
	}
	execRetry(t, ctx, `TRUNCATE sage.query_store_history`)
}
