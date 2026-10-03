package retention

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/partition"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// When all of the history partition must go for the cap and it covers no
// current time, it is dropped whole: one catalog change that returns its
// disk space at once, instead of deleting it row by row.
func TestSnapshotCap_DropsTheHistoryPartitionWhenAllOfItMustGo(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := partition.Snapshots
	bound := rebound(t, ctx, tbl, 3)
	t.Cleanup(func() { rebound(t, ctx, tbl, 3) })
	cleanCapRows(t, ctx)
	for h := 5; h >= 1; h-- {
		insertBlob(t, ctx, bound.Add(-time.Duration(h)*time.Hour), 64)
	}
	keep := []int64{insertBlob(t, ctx, bound.AddDate(0, 0, 1).Add(time.Hour), 64),
		insertBlob(t, ctx, partition.DayStart(time.Now()), 64)}
	before, err := partition.Size(ctx, pool, tbl)
	if err != nil {
		t.Fatal(err)
	}
	histDisk := partitionBytes(t, ctx, tbl.HistoryName())
	c := New(pool, snapshotCfg(), noopLog)
	// Room for the daily rows only (the slack absorbs a visibility map
	// autovacuum may add meanwhile): every history row must go.
	c.capBytes = before - histDisk + 16<<10
	stats := c.RunOnce(ctx)
	if !slices.Equal(stats.Dropped, []string{tbl.HistoryName()}) ||
		stats.Deleted["snapshots"] != 0 {
		t.Fatalf("stats = %+v, want only the history partition dropped, no row deleted", stats)
	}
	if relationExists(t, ctx, "sage."+tbl.HistoryName()) {
		t.Fatal("the history partition still exists")
	}
	if got := remainingIDs(t, ctx); !slices.Equal(got, keep) {
		t.Fatalf("remaining = %v, want the daily rows %v", got, keep)
	}
	after, err := partition.Size(ctx, pool, tbl)
	if err != nil || before-after < histDisk {
		t.Fatalf("size %d -> %d (%v): the history partition's %d bytes were not returned",
			before, after, err, histDisk)
	}
}

// An emptied history partition is dropped once it covers no current time;
// one that still covers today stays (writers are putting today's rows
// there), even empty.
func TestRunOnce_EmptyHistoryIsDroppedOnceItCoversNoCurrentTime(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := partition.Snapshots
	t.Cleanup(func() { rebound(t, ctx, tbl, 3) })
	cfg := snapshotCfg()
	rebound(t, ctx, tbl, -2)
	if stats := New(pool, cfg, noopLog).RunOnce(ctx); slices.Contains(stats.Dropped,
		tbl.HistoryName()) || !relationExists(t, ctx, "sage."+tbl.HistoryName()) {
		t.Fatalf("dropped %v: the history partition covering today must stay", stats.Dropped)
	}
	rebound(t, ctx, tbl, 3)
	stats := New(pool, cfg, noopLog).RunOnce(ctx)
	if !slices.Contains(stats.Dropped, tbl.HistoryName()) ||
		relationExists(t, ctx, "sage."+tbl.HistoryName()) {
		t.Fatalf("dropped %v, want the empty history partition", stats.Dropped)
	}
	// Rows older than every daily partition still have a home (the default
	// partition) and age out like the others.
	execRetry(t, ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES (now() - interval '100 days', 'cap_test', '{}')`)
	if again := New(pool, cfg, noopLog).RunOnce(ctx); again.Deleted["snapshots"] != 1 {
		t.Fatalf("deleted %d rows from the default partition, want 1",
			again.Deleted["snapshots"])
	}
}

// sage.query_store has no size cap; its history partition drains by age
// and is dropped once all of it is past the 14-day window.
func TestRunOnce_ExpiredQueryStoreHistoryIsDropped(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := partition.QueryStore
	rebound(t, ctx, tbl, 3)
	t.Cleanup(func() { rebound(t, ctx, tbl, 3) })
	execRetry(t, ctx, `INSERT INTO sage.query_store (captured_at, queryid, calls,
		total_exec_time, mean_exec_time)
		SELECT now() - interval '20 days', g, 1, 1, 1 FROM generate_series(1, 500) g`)
	cfg := &config.Config{Retention: config.RetentionConfig{QueryStoreDays: 14}}
	stats := New(pool, cfg, noopLog).RunOnce(ctx)
	if !slices.Contains(stats.Dropped, tbl.HistoryName()) ||
		stats.Deleted["query_store"] != 0 || relationExists(t, ctx, "sage."+tbl.HistoryName()) {
		t.Fatalf("stats = %+v, want the expired history partition dropped whole", stats)
	}
}

// A kept delta can reach the history partition through a checkpoint in a
// daily partition (keyframes before v1.8.3 were not day-aligned). The
// history partition must not be dropped while that chain is kept, even
// though no kept row names a history row directly.
func TestRunOnce_HistoryKeptWhileAKeptChainReachesIt(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := partition.Snapshots
	bound := rebound(t, ctx, tbl, 3)
	t.Cleanup(func() { rebound(t, ctx, tbl, 3) })
	cleanCapRows(t, ctx)
	k := insertBlob(t, ctx, bound.Add(-time.Hour), 4)
	cp := insertDelta(t, ctx, bound.Add(time.Hour), k)
	d := insertDelta(t, ctx, time.Now().Add(-time.Hour), cp)
	before := readable(t, ctx)
	if !slices.Contains(before, d) {
		t.Fatalf("delta %d is not readable before the run", d)
	}
	cfg := &config.Config{Retention: config.RetentionConfig{SnapshotsDays: 2}}
	stats := New(pool, cfg, noopLog).RunOnce(ctx)
	if slices.Contains(stats.Dropped, tbl.HistoryName()) {
		t.Fatalf("dropped the history partition holding %d, which %d -> %d still needs",
			k, cp, d)
	}
	assertStillReadable(t, ctx, before)
}

// The writer keeps writing while the trim deletes: no insert fails, every
// row it wrote stays and reads back, and the old rows still go.
func TestSnapshotCap_ConcurrentWriterNeverFails(t *testing.T) {
	pool, ctx := requireDB(t)
	rebound(t, ctx, partition.Snapshots, -2)
	cleanCapRows(t, ctx)
	today := partition.DayStart(time.Now())
	for i := 0; i < 150; i++ {
		insertBlob(t, ctx, today.AddDate(0, 0, -1).Add(time.Duration(i)*time.Minute), 8)
	}
	writer, err := pgxpool.New(ctx, testDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	done := make(chan struct{})
	var wg sync.WaitGroup
	var written []int64
	var werr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		written, werr = writeWhileTrimming(ctx, writer, done)
	}()
	c := New(pool, snapshotCfg(), noopLog).WithPacing(time.Millisecond, time.Minute)
	c.capBytes = 1
	stats := c.RunOnce(ctx)
	close(done)
	wg.Wait()
	if werr != nil || len(written) < 2 {
		t.Fatalf("writer: %d rows, %v", len(written), werr)
	}
	if stats.Deleted["snapshots"] != 150 {
		t.Fatalf("deleted %d rows, want the 150 old ones", stats.Deleted["snapshots"])
	}
	if got := remainingIDs(t, ctx); !slices.Equal(got, written) {
		t.Fatalf("remaining = %v, want exactly the writer's rows %v", got, written)
	}
	assertStillReadable(t, ctx, written)
}

// writeWhileTrimming writes a keyframe and deltas on it, as the collector
// does, until done (and at least twice).
func writeWhileTrimming(ctx context.Context, db *pgxpool.Pool, done <-chan struct{}) (
	[]int64, error) {
	var ids []int64
	for {
		var id int64
		var err error
		if len(ids) == 0 {
			err = db.QueryRow(ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
				VALUES (now(), 'cap_test', '[1]') RETURNING id`).Scan(&id)
		} else {
			err = db.QueryRow(ctx, `INSERT INTO sage.snapshots (collected_at, category, data,
				base_id) VALUES (now(), 'cap_test', '{"n": 1}', $1) RETURNING id`, ids[0]).
				Scan(&id)
		}
		if err != nil {
			return ids, fmt.Errorf("insert %d: %w", len(ids)+1, err)
		}
		ids = append(ids, id)
		select {
		case <-done:
			if len(ids) >= 2 {
				return ids, nil
			}
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// The trim walks the history partition's time index and deletes by TID,
// never scanning its heap (perf gate rule: no seq scan of a large table).
func TestTrimSQL_UsesTheTimeIndexAndTIDs(t *testing.T) {
	_, ctx := requireDB(t)
	tbl := partition.Snapshots
	rebound(t, ctx, tbl, -2)
	t.Cleanup(func() { execRetry(t, ctx, `TRUNCATE sage.snapshots`) })
	execRetry(t, ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		SELECT now() - interval '2 days' + g * interval '1 second', 'cap_test', '{}'
		FROM generate_series(1, 20000) g`)
	execRetry(t, ctx, `ANALYZE sage.snapshots_history`)
	plan, err := testdb.Explain(ctx, testPool, "", trimSQL(tbl.HistoryName(), snapshotBatchSize),
		time.Now().Add(-47*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var tid bool
	var walk func(n testdb.PlanNode) error
	walk = func(n testdb.PlanNode) error {
		if n.NodeType == "Seq Scan" && n.Relation == tbl.HistoryName() {
			return errors.New("seq scan of " + n.Relation)
		}
		tid = tid || n.NodeType == "Tid Scan"
		for _, c := range n.Plans {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(plan); err != nil || !tid {
		t.Fatalf("trim plan: %v (TID scan: %v)", err, tid)
	}
}

// capNotes: a state is logged when it changes, and repeated at most once
// per UTC day while it lasts. Being under the cap is logged only when the
// table comes back under it.
func TestCapNotes_LogOncePerStateChangeOrDay(t *testing.T) {
	n := &capNotes{}
	day := time.Date(2026, 10, 3, 14, 0, 0, 0, time.UTC)
	steps := []struct {
		state string
		at    time.Time
		want  bool
	}{
		{capUnder, day, false},                     // quiet start
		{"trimming", day, true},                    // went over
		{"trimming", day.Add(time.Hour), false},    // same state, same day
		{"stuck", day.Add(2 * time.Hour), true},    // changed
		{"stuck", day.Add(11 * time.Hour), true},   // next UTC day
		{"stuck", day.Add(12 * time.Hour), false},  // same day again
		{capUnder, day.Add(13 * time.Hour), true},  // back under
		{capUnder, day.Add(40 * time.Hour), false}, // under stays quiet
	}
	for i, s := range steps {
		if got := n.due("snapshots", s.state, s.at); got != s.want {
			t.Fatalf("step %d (%s at %s) = %v, want %v", i, s.state, s.at, got, s.want)
		}
	}
	if !n.due("query_store", "stuck", day) {
		t.Fatal("tables share their state")
	}
	var nilNotes *capNotes
	if nilNotes.due("snapshots", "stuck", day) != true {
		t.Fatal("a nil notes set must log everything rather than nothing")
	}
}

// capNotes is shared by a run and its control-database copy: concurrent
// use must be safe and log a state once.
func TestCapNotes_ConcurrentUseLogsOnce(t *testing.T) {
	n := &capNotes{}
	at := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	logged := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n.due("snapshots", "stuck", at) {
				mu.Lock()
				logged++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if logged != 1 {
		t.Fatalf("logged %d times, want once", logged)
	}
}

// The over-cap warnings name what is being done and how much is left.
func TestSnapshotCap_TrimWarningSaysWhatIsLeft(t *testing.T) {
	pool, ctx := requireDB(t)
	rebound(t, ctx, partition.Snapshots, -2)
	cleanCapRows(t, ctx)
	today := partition.DayStart(time.Now())
	for i := 0; i < 4; i++ {
		insertBlob(t, ctx, today.AddDate(0, 0, -1).Add(time.Duration(i)*time.Hour), 512)
	}
	logs := &captureLog{}
	c := New(pool, snapshotCfg(), logs.log)
	c.capBytes = 1
	c.RunOnce(ctx)
	for _, l := range logs.lines {
		if strings.HasPrefix(l, "WARN ") && strings.Contains(l, "trimming") {
			if !strings.Contains(l, "MB to go") {
				t.Fatalf("warning does not say how much is left: %s", l)
			}
			return
		}
	}
	t.Fatalf("no trimming warning: %v", logs.lines)
}

// Once its bound has passed, the history partition takes no new row, so
// space freed in it is never reused: only dropping it returns the space.
// The cap then counts its files, not its live rows: a closed history
// partition trimmed down to a few live rows but still large on disk is
// dropped, oldest data first, even though its live rows alone would fit.
func TestSnapshotCap_ClosedHistoryCountsItsFiles(t *testing.T) {
	pool, ctx := requireDB(t)
	tbl := partition.Snapshots
	bound := rebound(t, ctx, tbl, 1)
	t.Cleanup(func() { rebound(t, ctx, tbl, 3) })
	cleanCapRows(t, ctx)
	var ids []int64
	for h := 10; h >= 1; h-- {
		ids = append(ids, insertBlob(t, ctx, bound.Add(-time.Duration(h)*time.Hour), 64))
	}
	execRetry(t, ctx, `DELETE FROM sage.snapshots WHERE id = ANY($1)`, ids[:9]) // trimmed
	today := insertBlob(t, ctx, partition.DayStart(time.Now()), 64)
	disk, err := partition.Size(ctx, pool, tbl)
	if err != nil {
		t.Fatal(err)
	}
	histDisk := partitionBytes(t, ctx, tbl.HistoryName())
	logs := &captureLog{}
	c := New(pool, snapshotCfg(), logs.log)
	// Room for every live row, but not for the history partition's files.
	c.capBytes = disk - histDisk + 4*dataBytes(t, ctx, ids[9:])
	stats := c.RunOnce(ctx)
	if !slices.Contains(stats.Dropped, tbl.HistoryName()) || stats.Deleted["snapshots"] != 0 {
		t.Fatalf("stats = %+v, want the closed history partition dropped whole", stats)
	}
	if got := remainingIDs(t, ctx); !slices.Equal(got, []int64{today}) {
		t.Fatalf("remaining = %v, want today's row %d", got, today)
	}
	after, err := partition.Size(ctx, pool, tbl)
	if err != nil || after > c.capBytes {
		t.Fatalf("size after = %d (cap %d), %v", after, c.capBytes, err)
	}
}
