package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/partition"
)

// partitionDaysAhead keeps tomorrow's and the next days' partitions ready
// even when the collector (which ensures its own days) is not running.
const partitionDaysAhead = 3

// purgePartitioned applies a rule to a table partitioned by day: whole
// days older than the window are dropped (no row deleted, nothing left for
// vacuum), sage.snapshots is held under its size cap, and only the history
// and default partitions, which span many days, have rows deleted. It
// returns false when the run's deadline passed before it was done.
func (c *Cleaner) purgePartitioned(ctx context.Context, rule purgeRule, stats *RunStats,
	deadline time.Time) bool {
	t, ok := partitionedTable(rule)
	if !ok {
		c.logFn("ERROR", "retention: sage.%s is partitioned but has no partition rule",
			rule.table)
		return true
	}
	if _, err := partition.Ensure(ctx, c.pool, t, time.Now(), partitionDaysAhead); err != nil {
		c.logFn("WARN", "retention: ensure partitions of sage.%s: %v", t.Name, err)
	}
	cutoff := time.Now().Add(-time.Duration(rule.days) * 24 * time.Hour)
	c.dropExpiredDays(ctx, t, cutoff, stats)
	if t.Name == partition.Snapshots.Name && !c.enforceSnapshotCap(ctx, t, stats, deadline) {
		return false
	}
	parts, err := partition.List(ctx, c.pool, t)
	if err != nil {
		c.logFn("ERROR", "retention: purging sage.%s failed: %v", t.Name, err)
		return true
	}
	for _, p := range parts {
		if !p.History && !p.Default {
			continue
		}
		if p.History && c.dropDoneHistory(ctx, t, p, cutoff, stats) {
			continue
		}
		if !c.purgeRows(ctx, rule, "sage."+p.Name, stats, deadline) {
			return false
		}
	}
	return true
}

// dropExpiredDays drops the daily partitions whose every row is older than
// cutoff, unless a retained row still needs one of their rows as a base.
func (c *Cleaner) dropExpiredDays(ctx context.Context, t partition.Table, cutoff time.Time,
	stats *RunStats) {
	parts, err := partition.List(ctx, c.pool, t)
	if err != nil {
		c.logFn("ERROR", "retention: list partitions of sage.%s: %v", t.Name, err)
		return
	}
	for _, p := range parts {
		if p.History || p.Default || p.Upper.After(cutoff) {
			continue
		}
		if !c.removable(ctx, t, p, cutoff) {
			continue
		}
		if err := partition.Drop(ctx, c.pool, t, p); err != nil {
			c.logFn("WARN", "retention: drop expired %s: %v (retrying next run)", p.Name, err)
			continue
		}
		stats.Dropped = append(stats.Dropped, p.Name)
		c.logFn("INFO", "retention: dropped expired partition sage.%s", p.Name)
	}
}

// removable reports whether p can go: no row at or after retainedFrom is
// built on a row of p, directly or through a checkpoint built on one
// (chains are at most two deep, and before v1.8.3 a chain could cross
// midnight, so a checkpoint in a daily partition can rest on a keyframe in
// the history partition). Only snapshots have bases.
func (c *Cleaner) removable(ctx context.Context, t partition.Table, p partition.Partition,
	retainedFrom time.Time) bool {
	if t.Name != partition.Snapshots.Name {
		return true
	}
	var needed bool
	err := c.pool.QueryRow(ctx, fmt.Sprintf(`WITH ids AS (
		    SELECT min(id) AS lo, max(id) AS hi FROM %[1]s),
		on_p AS MATERIALIZED (
		    SELECT s.id, s.collected_at FROM sage.snapshots s, ids
		    WHERE s.base_id BETWEEN ids.lo AND ids.hi
		      AND EXISTS (SELECT 1 FROM %[1]s b WHERE b.id = s.base_id)),
		span AS (SELECT min(id) AS lo, max(id) AS hi FROM on_p)
		SELECT EXISTS (SELECT 1 FROM on_p WHERE collected_at >= $1)
		    OR EXISTS (SELECT 1 FROM sage.snapshots d, span
		               WHERE d.base_id BETWEEN span.lo AND span.hi
		                 AND d.collected_at >= $1
		                 AND d.base_id IN (SELECT id FROM on_p))`, ident(p.Name)),
		retainedFrom).Scan(&needed)
	if err != nil {
		c.logFn("WARN", "retention: check what needs sage.%s: %v", p.Name, err)
		return false
	}
	if needed {
		c.logFn("WARN", "retention: keeping sage.%s: retained snapshots are built on its "+
			"rows (it goes once they age out)", p.Name)
	}
	return !needed
}

// dropDoneHistory drops the history partition once it covers no current
// time (writers put no row in it any more) and is empty, or holds only
// rows past the window that no retained row is built on: one catalog
// change that returns its disk space at once, where a TRUNCATE would leave
// an empty partition behind and row deletes would free nothing. The drop
// re-checks under its lock that no row at or after cutoff arrived. A lock
// timeout is retried next run.
func (c *Cleaner) dropDoneHistory(ctx context.Context, t partition.Table, p partition.Partition,
	cutoff time.Time, stats *RunStats) bool {
	if p.Upper.After(time.Now()) {
		return false
	}
	var newest *time.Time
	if err := c.pool.QueryRow(ctx, fmt.Sprintf(`SELECT max(%s) FROM %s`,
		pgx.Identifier{t.Column}.Sanitize(), ident(p.Name))).Scan(&newest); err != nil {
		c.logFn("WARN", "retention: read the newest row of sage.%s: %v", p.Name, err)
		return false
	}
	if newest != nil && (!newest.Before(cutoff) || !c.removable(ctx, t, p, cutoff)) {
		return false
	}
	dropped, err := partition.DropHistory(ctx, c.pool, t, p, cutoff)
	if err != nil {
		c.logFn("WARN", "retention: drop %s: %v (retrying next run)", p.Name, err)
		return false
	}
	if dropped {
		stats.Dropped = append(stats.Dropped, p.Name)
		c.logFn("INFO", "retention: dropped sage.%s: it held no row newer than the "+
			"retention window (its disk space is returned)", p.Name)
	}
	return dropped
}
