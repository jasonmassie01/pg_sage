package retention

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/partition"
)

// The history partition holds every row written before sage.snapshots was
// partitioned (lifeos: 9.3 GB) and, until its upper bound, the rows written
// since. The size cap used to skip it while it covered today, and could
// otherwise only truncate all of it. These tests pin the trim: oldest rows
// first, in paced batches, never today's rows, never a base a kept row
// needs, measured by live data (a DELETE frees no disk space).

func snapshotCfg() *config.Config {
	return &config.Config{Retention: config.RetentionConfig{SnapshotsDays: 90}}
}

func scalar(t *testing.T, ctx context.Context, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := testPool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("query: %v\nsql: %s", err, sql)
	}
	return n
}

// dataBytes is what the rows' documents take as stored (compressed, TOAST).
func dataBytes(t *testing.T, ctx context.Context, ids []int64) int64 {
	t.Helper()
	return scalar(t, ctx, `SELECT COALESCE(sum(pg_column_size(data)), 0)::int8
		FROM sage.snapshots WHERE id = ANY($1)`, ids)
}

func remainingIDs(t *testing.T, ctx context.Context) []int64 {
	t.Helper()
	rows, err := testPool.Query(ctx, `SELECT id FROM sage.snapshots
		WHERE category = 'cap_test' ORDER BY collected_at, id`)
	if err != nil {
		t.Fatalf("remaining rows: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// readable lists the rows sage.snapshot_data can rebuild.
func readable(t *testing.T, ctx context.Context) []int64 {
	t.Helper()
	rows, err := testPool.Query(ctx, `SELECT id FROM sage.snapshots
		WHERE sage.snapshot_data(data, base_id) IS NOT NULL ORDER BY id`)
	if err != nil {
		t.Fatalf("readable rows: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	return out
}

// assertStillReadable fails when a row that was readable before is kept
// but no longer readable (its base, or its base's base, was deleted).
func assertStillReadable(t *testing.T, ctx context.Context, before []int64) {
	t.Helper()
	if n := scalar(t, ctx, `SELECT count(*) FROM sage.snapshots
		WHERE id = ANY($1) AND sage.snapshot_data(data, base_id) IS NULL`, before); n != 0 {
		t.Fatalf("%d kept rows lost their base", n)
	}
	assertNoOrphans(t)
}

func insertDelta(t *testing.T, ctx context.Context, at time.Time, base int64) int64 {
	t.Helper()
	return insertID(t, ctx, `INSERT INTO sage.snapshots (collected_at, category, data,
		base_id) VALUES ($1, 'cap_test', '{"n": 1}', $2) RETURNING id`, at, base)
}

func cleanCapRows(t *testing.T, ctx context.Context) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(),
			`DELETE FROM sage.snapshots WHERE category = 'cap_test'`)
	})
}

// Right after the upgrade (lifeos) the history partition covers today, so
// no daily partition can be dropped. Over the cap, its oldest rows are
// deleted until the live data fits: the six oldest of eleven rows here,
// never today's. The disk size barely moves (DELETE frees nothing until
// vacuum reuses it), so the cap must measure live data, or a second run
// would go on deleting rows the cap no longer needs gone.
func TestSnapshotCap_TrimsTheHistoryPartitionOldestFirst(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := partition.Snapshots
	rebound(t, ctx, tbl, -2)
	cleanCapRows(t, ctx)
	today := partition.DayStart(time.Now())
	var ids []int64
	for d := 10; d >= 1; d-- {
		ids = append(ids, insertBlob(t, ctx, today.AddDate(0, 0, -d).Add(time.Hour), 64))
	}
	ids = append(ids, insertBlob(t, ctx, today, 64))
	keep := ids[6:]
	smallest := scalar(t, ctx, `SELECT min(pg_column_size(data))::int8 FROM sage.snapshots
		WHERE id = ANY($1)`, ids)
	disk, err := partition.Size(ctx, pool, tbl)
	if err != nil {
		t.Fatal(err)
	}
	// The cap counts every partition: the other (empty) partitions' files
	// take their share of it.
	others := disk - partitionBytes(t, ctx, tbl.HistoryName())
	logs := &captureLog{}
	c := New(pool, snapshotCfg(), logs.log)
	c.capBytes = others + dataBytes(t, ctx, keep) + smallest/2
	stats := c.RunOnce(ctx)
	if got := remainingIDs(t, ctx); !slices.Equal(got, keep) {
		t.Fatalf("remaining = %v, want the five newest %v (deleted %d)", got, keep,
			stats.Deleted["snapshots"])
	}
	if stats.Deleted["snapshots"] != 6 || slices.Contains(stats.Dropped, tbl.HistoryName()) {
		t.Fatalf("stats = %+v, want 6 rows deleted and the history partition kept", stats)
	}
	if !logs.contains("WARN", "trimming the oldest rows of sage.snapshots_history") {
		t.Fatalf("the trim was not announced: %v", logs.lines)
	}
	disk, err = partition.Size(ctx, pool, tbl)
	if err != nil || disk <= c.capBytes {
		t.Fatalf("disk size %d (%v): precondition, deleted TOAST space is not returned "+
			"before vacuum, so the disk size stays over the %d cap", disk, err, c.capBytes)
	}
	again := c.RunOnce(ctx)
	if again.Deleted["snapshots"] != 0 || len(remainingIDs(t, ctx)) != len(keep) {
		t.Fatalf("second run deleted %d rows: the cap measured disk, not live data",
			again.Deleted["snapshots"])
	}
}

// Snapshots written before v1.8.3 have keyframes every 6 h that are not
// aligned on days: a delta after midnight can be built on a checkpoint
// before it, itself built on a keyframe before that. The trim stops at
// the oldest base a kept row needs (K1 here), even far over the cap, and
// every kept row stays readable.
func TestSnapshotCap_TrimStopsAtTheKeyframeBoundary(t *testing.T) {
	pool, ctx := requireDB(t)
	rebound(t, ctx, partition.Snapshots, -2)
	cleanCapRows(t, ctx)
	today := partition.DayStart(time.Now())
	y := today.AddDate(0, 0, -1)
	k0 := insertBlob(t, ctx, today.AddDate(0, 0, -3).Add(time.Hour), 16)
	d0 := insertDelta(t, ctx, today.AddDate(0, 0, -3).Add(2*time.Hour), k0)
	e := insertBlob(t, ctx, y.Add(18*time.Hour), 16)
	k1 := insertBlob(t, ctx, y.Add(20*time.Hour), 16)
	d1 := insertDelta(t, ctx, y.Add(21*time.Hour), k1)
	x := insertBlob(t, ctx, y.Add(21*time.Hour+30*time.Minute), 16)
	cp := insertDelta(t, ctx, y.Add(22*time.Hour), k1)
	d2 := insertDelta(t, ctx, today.Add(30*time.Minute), cp)
	last := insertBlob(t, ctx, today.Add(40*time.Minute), 16)
	before := readable(t, ctx)
	logs := &captureLog{}
	c := New(pool, snapshotCfg(), logs.log)
	c.capBytes = 1
	stats := c.RunOnce(ctx)
	want := []int64{k1, d1, x, cp, d2, last}
	if got := remainingIDs(t, ctx); !slices.Equal(got, want) {
		t.Fatalf("remaining = %v, want %v (K0 %d, its delta %d and E %d deleted)", got, want,
			k0, d0, e)
	}
	if stats.Deleted["snapshots"] != 3 {
		t.Fatalf("deleted %d rows, want 3", stats.Deleted["snapshots"])
	}
	assertStillReadable(t, ctx, before)
	if !logs.contains("WARN", "over its") {
		t.Fatalf("staying over the cap was not logged: %v", logs.lines)
	}
	if again := c.RunOnce(ctx); again.Deleted["snapshots"] != 0 {
		t.Fatalf("second run deleted %d rows past the boundary", again.Deleted["snapshots"])
	}
	assertStillReadable(t, ctx, before)
}

// The trim is paced and budgeted like every purge: one statement deletes
// at most snapshotBatchSize rows, a run stops when its budget is spent,
// and the next run resumes with sage.snapshots where the last one stopped,
// oldest rows first.
func TestSnapshotCap_HistoryTrimResumesAcrossRuns(t *testing.T) {
	pool, ctx := requireDB(t)
	rebound(t, ctx, partition.Snapshots, -2)
	cleanCapRows(t, ctx)
	today := partition.DayStart(time.Now())
	var ids []int64
	for i := 0; i < 120; i++ {
		ids = append(ids, insertBlob(t, ctx,
			today.AddDate(0, 0, -2).Add(time.Duration(i)*time.Minute), 4))
	}
	todays := insertBlob(t, ctx, today, 4)
	c := New(pool, snapshotCfg(), noopLog).WithPacing(time.Millisecond, time.Nanosecond)
	c.capBytes = 1
	for run, from := range []int{50, 100, 120} {
		stats := c.RunOnce(ctx)
		want := append(slices.Clone(ids[from:]), todays)
		if got := remainingIDs(t, ctx); !slices.Equal(got, want) {
			t.Fatalf("run %d: %d rows remain, want the %d newest (deleted %d)", run+1,
				len(got), len(want), stats.Deleted["snapshots"])
		}
		if run < 2 && (!stats.BudgetSpent || len(stats.Deferred) == 0 ||
			stats.Deferred[0] != "snapshots") {
			t.Fatalf("run %d = %+v, want the trim deferred to the next run", run+1, stats)
		}
	}
}

// A table under its cap is left alone: no row deleted, no partition
// dropped, nothing logged.
func TestSnapshotCap_UnderTheCapIsUntouched(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := partition.Snapshots
	rebound(t, ctx, tbl, -2)
	cleanCapRows(t, ctx)
	today := partition.DayStart(time.Now())
	for d := 5; d >= 1; d-- {
		insertBlob(t, ctx, today.AddDate(0, 0, -d), 16)
	}
	disk, err := partition.Size(ctx, pool, tbl)
	if err != nil {
		t.Fatal(err)
	}
	for _, capBytes := range []int64{disk * 10, 0} { // over the floor; off (no override)
		logs := &captureLog{}
		c := New(pool, snapshotCfg(), logs.log)
		c.capBytes = capBytes
		stats := c.RunOnce(ctx)
		if stats.Deleted["snapshots"] != 0 || len(stats.Dropped) != 0 ||
			len(remainingIDs(t, ctx)) != 5 {
			t.Fatalf("cap %d: stats = %+v, want nothing removed", capBytes, stats)
		}
		for _, l := range logs.lines {
			if strings.Contains(l, "cap") && strings.Contains(l, "snapshots") {
				t.Fatalf("cap %d: logged %q", capBytes, l)
			}
		}
	}
	if !relationExists(t, ctx, "sage."+tbl.HistoryName()) {
		t.Fatal("the history partition was dropped while it covers today")
	}
}

// Over the cap with nothing removable (all of it is today's), the warning
// is logged once, not on every run.
func TestSnapshotCap_WarningIsRateLimited(t *testing.T) {
	pool, ctx := requireDB(t)
	rebound(t, ctx, partition.Snapshots, -2)
	cleanCapRows(t, ctx)
	insertBlob(t, ctx, partition.DayStart(time.Now()), 16)
	logs := &captureLog{}
	c := New(pool, snapshotCfg(), logs.log)
	c.capBytes = 1
	for i := 0; i < 3; i++ {
		c.RunOnce(ctx)
	}
	warned := 0
	for _, l := range logs.lines {
		if strings.HasPrefix(l, "WARN ") && strings.Contains(l, "over its") {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("over-cap warning logged %d times in 3 runs, want once: %v", warned,
			logs.lines)
	}
	if !logs.contains("WARN", "retention.snapshots_max_pct") {
		t.Fatalf("the warning does not say what to do: %v", logs.lines)
	}
}

// The trim statement guards the bases of later rows on its own, for a row
// written after the boundary was chosen (an older writer still running):
// even told to delete everything before today, it keeps a checkpoint and
// keyframe that a row after midnight is built on.
func TestTrimSQL_KeepsBasesPastAnUnsafeBoundary(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := partition.Snapshots
	rebound(t, ctx, tbl, -2)
	cleanCapRows(t, ctx)
	today := partition.DayStart(time.Now())
	y := today.AddDate(0, 0, -1)
	k := insertBlob(t, ctx, y.Add(20*time.Hour), 4)
	leaf := insertDelta(t, ctx, y.Add(21*time.Hour), k)
	cp := insertDelta(t, ctx, y.Add(22*time.Hour), k)
	d := insertDelta(t, ctx, today.Add(30*time.Minute), cp)
	before := readable(t, ctx)
	c := New(pool, snapshotCfg(), noopLog)
	stats := newRunStats()
	h := partition.Partition{Name: tbl.HistoryName(), History: true}
	r := c.deleteBefore(ctx, h, today, &stats, time.Now().Add(time.Minute))
	if !r.done || r.rows != 1 {
		t.Fatalf("deleted %d rows (done %v), want only the leaf %d", r.rows, r.done, leaf)
	}
	if got := remainingIDs(t, ctx); !slices.Equal(got, []int64{k, cp, d}) {
		t.Fatalf("remaining = %v, want %v", got, []int64{k, cp, d})
	}
	assertStillReadable(t, ctx, before)
}

// Each run trims at most trimBudget bytes of documents (a DELETE of TOAST
// rows writes about as much WAL, full-page images included), checked after
// each statement. Reaching it ends the trim for this run without spending
// the run: the other tables are still purged, and the next run goes on.
func TestSnapshotCap_TrimStopsAtItsByteBudgetPerRun(t *testing.T) {
	pool, ctx := requireDB(t)
	rebound(t, ctx, partition.Snapshots, -2)
	cleanCapRows(t, ctx)
	today := partition.DayStart(time.Now())
	var ids []int64
	for i := 0; i < 120; i++ {
		ids = append(ids, insertBlob(t, ctx,
			today.AddDate(0, 0, -2).Add(time.Duration(i)*time.Minute), 4))
	}
	batch := dataBytes(t, ctx, ids[:snapshotBatchSize])
	logs := &captureLog{}
	c := New(pool, snapshotCfg(), logs.log)
	c.capBytes = 1
	c.trimBudget = batch + batch/2 // the second statement crosses it
	first := c.RunOnce(ctx)
	if first.Deleted["snapshots"] != 100 || first.BudgetSpent {
		t.Fatalf("first run = %+v, want 100 rows and the run's time budget unspent", first)
	}
	if got := remainingIDs(t, ctx); !slices.Equal(got, ids[100:]) {
		t.Fatalf("remaining = %v, want the 20 newest", got)
	}
	if logs.contains("WARN", "nothing older than today") {
		t.Fatalf("a paused trim was reported as stuck: %v", logs.lines)
	}
	if second := c.RunOnce(ctx); second.Deleted["snapshots"] != 20 {
		t.Fatalf("second run deleted %d rows, want the last 20", second.Deleted["snapshots"])
	}
	if New(nil, snapshotCfg(), noopLog).trimBudget != defaultTrimBudget {
		t.Fatal("a new cleaner does not use the default trim budget")
	}
}
