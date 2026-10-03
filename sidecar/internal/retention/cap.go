package retention

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/partition"
)

// minSnapshotCapBytes is the smallest snapshot size cap: on a small
// database a percentage would leave too little history to forecast from.
const minSnapshotCapBytes int64 = 256 << 20

// snapshotCap is the size sage.snapshots may take in a database of
// dbBytes: retention.snapshots_max_pct of it, never below the floor; 0
// when the cap is off.
func (c *Cleaner) snapshotCap(dbBytes int64) int64 {
	pct := int64(c.cfg.Retention.SnapshotsMaxPct)
	if pct <= 0 {
		return 0
	}
	return max(dbBytes*pct/100, minSnapshotCapBytes)
}

// capFor is the cap for a table of size bytes, reading the database size
// only when the table is over the floor (pg_database_size stats every
// relation file: 150 ms on a 15k-table database).
func (c *Cleaner) capFor(ctx context.Context, size int64) (int64, bool) {
	if c.capBytes > 0 {
		return c.capBytes, true
	}
	if c.cfg.Retention.SnapshotsMaxPct <= 0 || size <= minSnapshotCapBytes {
		return 0, false
	}
	var dbBytes int64
	if err := c.pool.QueryRow(ctx,
		`SELECT pg_catalog.pg_database_size(pg_catalog.current_database())`).
		Scan(&dbBytes); err != nil {
		c.logFn("WARN", "retention: read the database size for the snapshot cap: %v", err)
		return 0, false
	}
	return c.snapshotCap(dbBytes), true
}

// enforceSnapshotCap holds sage.snapshots under its size cap, oldest data
// first: the history partition's oldest rows are trimmed (all of it is
// dropped when all of it must go), then whole days are dropped. Today's
// rows, today's and later partitions, the default partition, and every
// row a kept row is built on stay. The cap counts live data (capUsage), so
// trimmed rows count as gone although their space is only reused, not
// returned, until the history partition is dropped. It returns false when
// the run's deadline cut the trim short (the next run resumes it).
func (c *Cleaner) enforceSnapshotCap(ctx context.Context, t partition.Table,
	stats *RunStats, deadline time.Time) bool {
	disk, err := partition.Size(ctx, c.pool, t)
	if err != nil {
		c.logFn("WARN", "retention: size of sage.%s: %v", t.Name, err)
		return true
	}
	limit, on := c.capFor(ctx, disk)
	if !on {
		return true
	}
	if disk <= limit { // live data is never more than the files
		c.noteUnder(t, capUsage{disk: disk, live: disk}, limit)
		return true
	}
	u, err := c.measure(ctx, t)
	if err != nil {
		c.logFn("WARN", "retention: measure sage.%s for its size cap: %v", t.Name, err)
		return true
	}
	if u.live <= limit {
		c.noteUnder(t, u, limit)
		return true
	}
	if u.hist != nil && u.histRows > 0 {
		done, settled := c.trimHistory(ctx, t, u, limit, stats, deadline)
		if !done || settled {
			return done
		}
		if u, err = c.measure(ctx, t); err != nil {
			c.logFn("WARN", "retention: measure sage.%s for its size cap: %v", t.Name, err)
			return true
		}
		if u.hist != nil && u.histRows > 0 {
			c.noteStuck(t, u, limit)
			return true
		}
	}
	c.dropDaysForCap(ctx, t, u, limit, stats)
	return true
}

// dropDaysForCap drops whole days, oldest first, until the table fits. A
// day a kept row is built on is not dropped: the cap waits until those
// rows go (logged).
func (c *Cleaner) dropDaysForCap(ctx context.Context, t partition.Table, u capUsage,
	limit int64, stats *RunStats) {
	parts, err := partition.List(ctx, c.pool, t)
	if err != nil {
		c.logFn("WARN", "retention: list partitions of sage.%s: %v", t.Name, err)
		return
	}
	today := partition.DayStart(time.Now())
	for _, p := range parts {
		if p.History {
			continue
		}
		if u.live <= limit || p.Default || p.Upper.After(today) {
			break
		}
		if !c.removable(ctx, t, p, p.Upper) || !c.dropDayForCap(ctx, t, p, stats) {
			break
		}
		if u, err = c.measure(ctx, t); err != nil {
			c.logFn("WARN", "retention: measure sage.%s for its size cap: %v", t.Name, err)
			return
		}
	}
	if u.live > limit {
		c.noteStuck(t, u, limit)
		return
	}
	c.noteUnder(t, u, limit)
}

// dropDayForCap drops one day's partition.
func (c *Cleaner) dropDayForCap(ctx context.Context, t partition.Table, p partition.Partition,
	stats *RunStats) bool {
	if err := partition.Drop(ctx, c.pool, t, p); err != nil {
		c.logFn("WARN", "retention: removing %s for the size cap: %v (retrying next run)",
			p.Name, err)
		return false
	}
	stats.Dropped = append(stats.Dropped, p.Name)
	c.logFn("INFO", "retention: removed sage.%s to keep sage.%s under its size cap",
		p.Name, t.Name)
	return true
}

// noteUnder logs, once, that a table is back under its cap.
func (c *Cleaner) noteUnder(t partition.Table, u capUsage, limit int64) {
	c.note(t.Name, capUnder, "INFO", "retention: sage.%s is back under its %d MB size cap "+
		"(%d MB of live data, %d MB on disk: space freed by trimming is reused by new rows "+
		"of the same partition and returned to the operating system when it is dropped)",
		t.Name, limit>>20, u.live>>20, u.disk>>20)
}

// noteStuck warns, once per state change or day, that a table is over its
// cap with nothing left that may go yet.
func (c *Cleaner) noteStuck(t partition.Table, u capUsage, limit int64) {
	c.note(t.Name, "stuck", "WARN", "retention: sage.%s is %d MB, over its %d MB cap, with "+
		"nothing older than today left to remove (the rest is today's or a base today's "+
		"snapshots are built on). It shrinks as days age out; if this repeats every day, "+
		"raise retention.snapshots_max_pct or lower retention.snapshots_days",
		t.Name, u.live>>20, limit>>20)
}
