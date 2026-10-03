package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/partition"
)

// The history partition of sage.snapshots holds every row written before
// the table was partitioned by day (lifeos: 9.3 GB) and, until its upper
// bound, the rows written since. No day can be dropped before it, so the
// size cap trims it: its oldest rows are deleted in paced batches (TID
// arrays, a pause between statements, the run's budget, resumed by the
// next run) until the live data fits, and all of it is dropped at once
// when all of it must go.
//
// Space: a DELETE frees no disk space. Autovacuum makes the trimmed space
// reusable, but only by rows of the same partition, and the history
// partition takes no new row once its bound has passed; the space goes
// back to the operating system when the partition is dropped (once empty,
// or once wholly past retention.snapshots_days: dropDoneHistory). VACUUM
// FULL is never run: it would lock the table for the whole rewrite. The
// cap therefore counts live data (capUsage), not files, or it would go on
// deleting rows long after enough of them were gone.

// rowOverheadBytes estimates what a snapshot row takes beside its stored
// document: the heap tuple (header, id, collected_at, category, base_id,
// TOAST pointer: about 100 bytes) and its entries in the four indexes
// (about 25 bytes each). TOAST chunk headers add about 2% of the document,
// which the estimate leaves out.
const rowOverheadBytes = 200

// maxBoundarySteps bounds the walk back to a keyframe boundary. The writer
// chains rows at most two deep (delta, checkpoint, keyframe), so the walk
// takes at most three steps.
const maxBoundarySteps = 16

// capUsage is a day-partitioned table's size as the cap counts it.
type capUsage struct {
	disk     int64                // every partition's files
	live     int64                // disk, with the history partition's live rows for its files
	hist     *partition.Partition // nil when there is none
	histDisk int64
	histLive int64 // estimated: stored documents plus rowOverheadBytes a row
	histRows int64
}

// measure sizes t for its cap. Daily partitions lose no rows (they are
// dropped whole), so their files are their size; the history partition's
// live rows are estimated from pg_column_size, which reads a TOASTed
// value's stored size from its pointer without fetching it: one scan of
// the partition's heap (lifeos: 50 MB, 26 ms).
func (c *Cleaner) measure(ctx context.Context, t partition.Table) (capUsage, error) {
	var u capUsage
	disk, err := partition.Size(ctx, c.pool, t)
	if err != nil {
		return u, err
	}
	parts, err := partition.List(ctx, c.pool, t)
	if err != nil {
		return u, err
	}
	u.disk, u.live = disk, disk
	for i := range parts {
		if parts[i].History {
			u.hist = &parts[i]
		}
	}
	if u.hist == nil {
		return u, nil
	}
	err = c.pool.QueryRow(ctx, fmt.Sprintf(`SELECT
		pg_catalog.pg_total_relation_size(pg_catalog.to_regclass($1)), count(*),
		COALESCE(sum(pg_catalog.pg_column_size(data)::int8 + $2), 0)::int8 FROM %s`,
		ident(u.hist.Name)), "sage."+u.hist.Name, rowOverheadBytes).
		Scan(&u.histDisk, &u.histRows, &u.histLive)
	if err != nil {
		return u, fmt.Errorf("live rows of sage.%s: %w", u.hist.Name, err)
	}
	u.live = disk - u.histDisk + u.histLive
	return u, nil
}

// trimHistory deletes the history partition's oldest rows until the table
// fits its cap, never a row of today (UTC) and never a row a kept row is
// built on (safeBoundary). done is false when the run's deadline cut it
// short; fits reports that the table is now under its cap.
func (c *Cleaner) trimHistory(ctx context.Context, t partition.Table, u capUsage,
	limit int64, stats *RunStats, deadline time.Time) (done, fits bool) {
	h, now := *u.hist, time.Now()
	stop := partition.DayStart(now)
	if h.Upper.Before(stop) {
		stop = h.Upper
	}
	excess := u.live - limit
	b, err := c.trimTarget(ctx, h, stop, excess)
	if err == nil {
		b, err = c.safeBoundary(ctx, h, b)
	}
	if err != nil {
		c.logFn("WARN", "retention: choose what to trim from sage.%s: %v (retrying next run)",
			h.Name, err)
		return true, false
	}
	if c.dropTrimmedHistory(ctx, t, h, b, now, stats) {
		rest := capUsage{disk: u.disk - u.histDisk, live: u.live - u.histLive}
		if rest.live <= limit {
			c.noteUnder(t, rest, limit)
		}
		return true, rest.live <= limit
	}
	if !c.anyBefore(ctx, h, b) {
		return true, false
	}
	c.note(t.Name, "trimming", "WARN", "retention: sage.%s is %d MB (%d MB on disk), over "+
		"its %d MB cap: trimming the oldest rows of sage.%s in paced batches, %d MB to go",
		t.Name, u.live>>20, u.disk>>20, limit>>20, h.Name, excess>>20)
	rows, bytes, done := c.deleteBefore(ctx, h, b, stats, deadline)
	left := excess - bytes - rows*rowOverheadBytes
	if rows > 0 {
		c.logFn("INFO", "retention: trimmed %d rows (%d MB) from sage.%s for the size cap; "+
			"%d MB to go", rows, bytes>>20, h.Name, max(left, 0)>>20)
	}
	if done && left <= 0 {
		c.noteUnder(t, capUsage{disk: u.disk, live: u.live - (excess - left)}, limit)
	}
	return done, done && left <= 0
}

// trimTarget is the boundary that frees excess bytes: rows collected
// before it go. It is just after the row whose deletion, oldest first,
// reaches excess, and never later than stop.
func (c *Cleaner) trimTarget(ctx context.Context, h partition.Partition, stop time.Time,
	excess int64) (time.Time, error) {
	var at *time.Time
	err := c.pool.QueryRow(ctx, fmt.Sprintf(`SELECT min(collected_at) FROM (
		SELECT collected_at, sum(pg_catalog.pg_column_size(data)::int8 + $3)
		       OVER (ORDER BY collected_at, id) AS freed
		FROM %s WHERE collected_at < $1) s
		WHERE freed >= $2`, ident(h.Name)), stop, excess, rowOverheadBytes).Scan(&at)
	if err != nil {
		return stop, fmt.Errorf("size the oldest rows: %w", err)
	}
	if at == nil || !at.Add(time.Microsecond).Before(stop) {
		return stop, nil
	}
	return at.Add(time.Microsecond), nil
}

// safeBoundary moves b back until no row collected at or after b is built
// on a row before it: rows before the oldest base a kept row needs go,
// that base and everything after it stay. Rows written before v1.8.3 have
// keyframes every 6 h that are not aligned on days, so a delta after
// midnight can need a checkpoint and a keyframe from the evening before.
func (c *Cleaner) safeBoundary(ctx context.Context, h partition.Partition, b time.Time) (
	time.Time, error) {
	query := fmt.Sprintf(`WITH gone AS (
		    SELECT min(id) AS lo, max(id) AS hi FROM %[1]s WHERE collected_at < $1)
		SELECT min(base.collected_at) FROM gone, sage.snapshots d
		JOIN %[1]s base ON base.id = d.base_id
		WHERE d.base_id BETWEEN gone.lo AND gone.hi
		  AND d.collected_at >= $1 AND base.collected_at < $1`, ident(h.Name))
	for i := 0; i < maxBoundarySteps; i++ {
		var need *time.Time
		if err := c.pool.QueryRow(ctx, query, b).Scan(&need); err != nil {
			return b, fmt.Errorf("find the bases kept rows need: %w", err)
		}
		if need == nil || !need.Before(b) {
			return b, nil
		}
		b = *need
	}
	return b, fmt.Errorf("no keyframe boundary after %d steps back (chains deeper than "+
		"the writer makes)", maxBoundarySteps)
}

// dropTrimmedHistory drops the history partition when the trim would
// leave nothing in it and it covers no current time: one catalog change
// that returns the space at once instead of deleting row by row. A lock
// timeout leaves it to the row-by-row trim and the next run.
func (c *Cleaner) dropTrimmedHistory(ctx context.Context, t partition.Table,
	h partition.Partition, b, now time.Time, stats *RunStats) bool {
	if h.Upper.After(now) {
		return false
	}
	var rest bool
	if err := c.pool.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s
		WHERE collected_at >= $1)`, ident(h.Name)), b).Scan(&rest); err != nil || rest {
		return false
	}
	dropped, err := partition.DropHistory(ctx, c.pool, t, h, b)
	if err != nil {
		c.logFn("WARN", "retention: dropping sage.%s for the size cap: %v (trimming its rows "+
			"instead; the drop is retried once it is empty)", h.Name, err)
		return false
	}
	if dropped {
		stats.Dropped = append(stats.Dropped, h.Name)
		c.logFn("INFO", "retention: dropped sage.%s to keep sage.%s under its size cap "+
			"(its disk space is returned)", h.Name, t.Name)
	}
	return dropped
}

// anyBefore reports whether the history partition has a row before b.
func (c *Cleaner) anyBefore(ctx context.Context, h partition.Partition, b time.Time) bool {
	var some bool
	err := c.pool.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s
		WHERE collected_at < $1)`, ident(h.Name)), b).Scan(&some)
	if err != nil {
		c.logFn("WARN", "retention: look for rows to trim in sage.%s: %v", h.Name, err)
	}
	return err == nil && some
}

// deleteBefore deletes the history partition's rows collected before b,
// oldest first, in paced batches. It returns the rows deleted, their
// stored size, and false when the run's deadline cut it short.
func (c *Cleaner) deleteBefore(ctx context.Context, h partition.Partition, b time.Time,
	stats *RunStats, deadline time.Time) (rows, bytes int64, done bool) {
	query := trimSQL(h.Name, snapshotBatchSize)
	done, err := c.paced(ctx, snapshotBatchSize, deadline, func() (int64, error) {
		var n, freed int64
		if err := c.pool.QueryRow(ctx, query, b).Scan(&n, &freed); err != nil {
			return 0, err
		}
		stats.count(partition.Snapshots.Name, n)
		rows, bytes = rows+n, bytes+freed
		return n, nil
	})
	if err != nil {
		c.logFn("ERROR", "retention: trimming sage.%s for the size cap failed after %d rows: "+
			"%v (retrying next run)", h.Name, rows, err)
	}
	return rows, bytes, done
}

// trimSQL deletes one batch of the history partition's rows collected
// before $1, oldest first along its time index, by TID (see purgeSQL). The
// keep predicate is a guard: safeBoundary already chose $1 so that no
// later row is built on these, but a row written since (an older writer
// still running) must not lose its base either.
func trimSQL(hist string, batch int) string {
	return fmt.Sprintf(`WITH gone AS (DELETE FROM %[1]s WHERE ctid = ANY (ARRAY(
		    SELECT ctid FROM %[1]s AS snapshots
		    WHERE collected_at < $1
		    %[2]s
		    ORDER BY collected_at LIMIT %[3]d))
		    RETURNING pg_catalog.pg_column_size(data)::int8 AS bytes)
		SELECT count(*), COALESCE(sum(bytes), 0)::int8 FROM gone`,
		ident(hist), keepSnapshotBases("$1"), batch)
}

// ident is sage.<name>, quoted.
func ident(name string) string { return pgx.Identifier{"sage", name}.Sanitize() }
