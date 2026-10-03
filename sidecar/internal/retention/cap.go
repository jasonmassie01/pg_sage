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

// enforceSnapshotCap removes the oldest snapshot data until the table fits
// its cap: the history partition is truncated first, then whole days are
// dropped, oldest first. Today's and later partitions and the default
// partition stay. A partition a remaining row is built on is never
// removed: the cap waits until those rows go (logged).
func (c *Cleaner) enforceSnapshotCap(ctx context.Context, t partition.Table,
	stats *RunStats) {
	size, err := partition.Size(ctx, c.pool, t)
	if err != nil {
		c.logFn("WARN", "retention: size of sage.%s: %v", t.Name, err)
		return
	}
	limit, on := c.capFor(ctx, size)
	if !on || size <= limit {
		return
	}
	parts, err := partition.List(ctx, c.pool, t)
	if err != nil {
		c.logFn("WARN", "retention: list partitions of sage.%s: %v", t.Name, err)
		return
	}
	today := partition.DayStart(time.Now())
	for _, p := range parts {
		if size <= limit || p.Default || p.Upper.After(today) {
			break
		}
		if p.History && c.empty(ctx, p) {
			continue
		}
		if !c.removable(ctx, t, p, p.Upper) {
			return
		}
		if !c.removeForCap(ctx, t, p, stats) {
			return
		}
		if size, err = partition.Size(ctx, c.pool, t); err != nil {
			c.logFn("WARN", "retention: size of sage.%s: %v", t.Name, err)
			return
		}
	}
	if size > limit {
		c.logFn("WARN", "retention: sage.%s is %d MB, over its %d MB cap, with nothing older "+
			"than today left to remove", t.Name, size>>20, limit>>20)
	}
}

// removeForCap truncates the history partition or drops a day.
func (c *Cleaner) removeForCap(ctx context.Context, t partition.Table, p partition.Partition,
	stats *RunStats) bool {
	var err error
	if p.History {
		err = partition.Truncate(ctx, c.pool, t, p)
	} else {
		err = partition.Drop(ctx, c.pool, t, p)
	}
	if err != nil {
		c.logFn("WARN", "retention: removing %s for the size cap: %v (retrying next run)",
			p.Name, err)
		return false
	}
	if p.History {
		stats.Truncated = append(stats.Truncated, p.Name)
	} else {
		stats.Dropped = append(stats.Dropped, p.Name)
	}
	c.logFn("INFO", "retention: removed sage.%s to keep sage.%s under its size cap",
		p.Name, t.Name)
	return true
}

// empty reports whether a partition holds no row (or cannot be read).
func (c *Cleaner) empty(ctx context.Context, p partition.Partition) bool {
	var rows bool
	err := c.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM sage."+p.Name+")").Scan(&rows)
	return err != nil || !rows
}
