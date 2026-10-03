package retention

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/partition"
)

// The history partition of sage.snapshots holds every row written before
// the table was partitioned by day (lifeos: 9.3 GB) and, until its upper
// bound (the second UTC midnight after the conversion), the rows written
// since. While it is open (its bound is ahead), writers still put today's
// rows in it and no day can be dropped before it, so the size cap trims
// it: its oldest rows are deleted in paced batches (TID arrays, a pause
// between statements, the run's time and byte budgets, resumed by a later
// run) until the live data fits, unless it closes within trimSkipWindow.
// Once it is closed, the cap drops it whole.
//
// Space: a DELETE frees no disk space. Autovacuum makes the trimmed space
// reusable, but only by rows of the same partition: the rows still landing
// in an open history partition reuse it, a closed one takes no new row, so
// its freed space is dead weight until the partition is dropped. VACUUM
// FULL is never run (it would lock the table for the whole rewrite). The
// cap therefore counts an open history partition by its live rows, or it
// would go on deleting rows long after enough of them were gone, and a
// closed one by its files, which only a drop returns: once closed, a
// history partition over the cap is dropped (dropClosedHistory), and one
// under it is dropped once empty or wholly expired (dropDoneHistory).

// rowOverheadBytes estimates what a snapshot row takes beside its stored
// document: the heap tuple (header, id, collected_at, category, base_id,
// TOAST pointer: about 100 bytes) and its entries in the four indexes
// (about 25 bytes each). TOAST chunk headers add about 2% of the document,
// which the estimate leaves out.
const rowOverheadBytes = 200

// defaultTrimBudget bounds the documents one run trims. Deleting TOAST rows
// writes about as much WAL as it deletes (each page's first change after a
// checkpoint is logged in full), so a run frees at most about one default
// max_wal_size; a 6 GB backlog takes several runs instead of one burst.
const defaultTrimBudget int64 = 1 << 30

// errTrimBudget ends a run's trim once its byte budget is spent.
var errTrimBudget = errors.New("trim budget spent")

// maxBoundarySteps bounds the walk back to a keyframe boundary. The writer
// chains rows at most two deep (delta, checkpoint, keyframe), so the walk
// takes at most three steps.
const maxBoundarySteps = 16

// capUsage is a day-partitioned table's size as the cap counts it.
type capUsage struct {
	disk     int64                // every partition's files
	live     int64                // what the cap counts: disk, less what trimming freed
	hist     *partition.Partition // nil when there is none
	histDisk int64
	histLive int64 // open: documents plus rowOverheadBytes a row; closed: its files
	histRows int64 // open: its rows; closed: 1 when it holds any
}

// open reports whether the history partition still takes new rows.
func (u capUsage) open(now time.Time) bool { return u.hist != nil && u.hist.Upper.After(now) }

// measure sizes t for its cap. Daily partitions lose no rows (they are
// dropped whole), so their files are their size, and so are a closed
// history partition's. An open history partition's live rows are
// estimated from pg_column_size, which reads a TOASTed value's stored size
// from its pointer without fetching it: one scan of the partition's heap
// (lifeos: 50 MB, 26 ms).
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
	if err := c.measureHistory(ctx, &u); err != nil {
		return u, fmt.Errorf("measure sage.%s: %w", u.hist.Name, err)
	}
	u.live = disk - u.histDisk + u.histLive
	return u, nil
}

// measureHistory sizes the history partition: an open one by its live rows,
// a closed one by its files (with whether it holds any row).
func (c *Cleaner) measureHistory(ctx context.Context, u *capUsage) error {
	name := "sage." + u.hist.Name
	if !u.open(time.Now()) {
		err := c.pool.QueryRow(ctx, fmt.Sprintf(`SELECT
			pg_catalog.pg_total_relation_size(pg_catalog.to_regclass($1)),
			(SELECT count(*) FROM (SELECT 1 FROM %s LIMIT 1) s)`, ident(u.hist.Name)), name).
			Scan(&u.histDisk, &u.histRows)
		u.histLive = u.histDisk
		return err
	}
	return c.pool.QueryRow(ctx, fmt.Sprintf(`SELECT
		pg_catalog.pg_total_relation_size(pg_catalog.to_regclass($1)), count(*),
		COALESCE(sum(pg_catalog.pg_column_size(data)::int8 + $2), 0)::int8 FROM %s`,
		ident(u.hist.Name)), name, rowOverheadBytes).
		Scan(&u.histDisk, &u.histRows, &u.histLive)
}

// trimHistory deletes an open history partition's oldest rows until the
// table fits its cap, never a row of today (UTC) and never a row a kept row is
// built on (safeBoundary). done is false when the run's deadline cut it
// short; settled reports that the cap needs nothing more this run (the
// table fits, or the run's trim budget is spent).
func (c *Cleaner) trimHistory(ctx context.Context, t partition.Table, u capUsage,
	limit int64, stats *RunStats, deadline time.Time) (done, settled bool) {
	h := *u.hist
	stop := partition.DayStart(time.Now()) // an open partition covers today
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
	if !c.anyBefore(ctx, h, b) {
		return true, false
	}
	c.note(t.Name, "trimming", "WARN", "retention: sage.%s is %d MB (%d MB on disk), over "+
		"its %d MB cap: trimming the oldest rows of sage.%s in paced batches, %d MB to go. "+
		"Rows written until %s reuse the space; the rest returns to the operating system "+
		"when the partition is dropped after that", t.Name, u.live>>20, u.disk>>20,
		limit>>20, h.Name, excess>>20, h.Upper.Format(time.RFC3339))
	r := c.deleteBefore(ctx, h, b, stats, deadline)
	left := excess - r.bytes - r.rows*rowOverheadBytes
	if r.rows > 0 {
		c.logFn("INFO", "retention: trimmed %d rows (%d MB) from sage.%s for the size cap; "+
			"%d MB to go", r.rows, r.bytes>>20, h.Name, max(left, 0)>>20)
	}
	fits := r.done && left <= 0
	if fits {
		c.noteUnder(t, capUsage{disk: u.disk, live: u.live - (excess - left)}, limit)
	}
	return r.done, fits || r.paused
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

// dropClosedHistory drops a closed history partition for the cap: its
// rows are the oldest, and deleting them would free no space. It stays
// while a kept row is built on one of its rows (logged by removable), and
// a lock timeout leaves it for the next run.
func (c *Cleaner) dropClosedHistory(ctx context.Context, t partition.Table,
	h partition.Partition, stats *RunStats) bool {
	if !c.removable(ctx, t, h, h.Upper) {
		return false
	}
	dropped, err := partition.DropHistory(ctx, c.pool, t, h, h.Upper)
	if err != nil {
		c.logFn("WARN", "retention: dropping sage.%s for the size cap: %v (retrying next run)",
			h.Name, err)
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

// trimmed is what one run's trim did: rows deleted and their stored size;
// done is false when the run's deadline cut it short, paused is true when
// it stopped at the run's trim budget.
type trimmed struct {
	rows, bytes  int64
	done, paused bool
}

// deleteBefore deletes the history partition's rows collected before b,
// oldest first, in paced batches, until none is left, the run's deadline
// passes, or the run's trim budget is spent.
func (c *Cleaner) deleteBefore(ctx context.Context, h partition.Partition, b time.Time,
	stats *RunStats, deadline time.Time) trimmed {
	var r trimmed
	query := trimSQL(h.Name, snapshotBatchSize)
	done, err := c.paced(ctx, snapshotBatchSize, deadline, func() (int64, error) {
		var n, freed int64
		if err := c.pool.QueryRow(ctx, query, b).Scan(&n, &freed); err != nil {
			return 0, err
		}
		stats.count(partition.Snapshots.Name, n)
		r.rows, r.bytes = r.rows+n, r.bytes+freed
		if n == snapshotBatchSize && c.trimBudget > 0 && r.bytes >= c.trimBudget {
			return n, errTrimBudget
		}
		return n, nil
	})
	r.done, r.paused = done, errors.Is(err, errTrimBudget)
	if err != nil && !r.paused {
		c.logFn("ERROR", "retention: trimming sage.%s for the size cap failed after %d rows: "+
			"%v (retrying next run)", h.Name, r.rows, err)
	}
	return r
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
