package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/partition"
)

// partitionDaysAhead keeps tomorrow's and the next days' partitions ready
// even when the collector (which ensures its own days) is not running.
const partitionDaysAhead = 3

// purgePartitioned applies a rule to a table partitioned by day: whole
// days older than the window are dropped (no row deleted, nothing left for
// vacuum), sage.snapshots is held under its size cap, and only the history
// and default partitions, which span many days, have rows deleted.
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
	if t.Name == partition.Snapshots.Name {
		c.enforceSnapshotCap(ctx, t, stats)
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
		if p.History && c.truncateExpired(ctx, t, p, cutoff, stats) {
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

// removable reports whether p can go: no row at or after retainedFrom
// names a row of p as its base. Only snapshots have bases.
func (c *Cleaner) removable(ctx context.Context, t partition.Table, p partition.Partition,
	retainedFrom time.Time) bool {
	if t.Name != partition.Snapshots.Name {
		return true
	}
	var needed bool
	err := c.pool.QueryRow(ctx, fmt.Sprintf(`WITH ids AS (
		    SELECT min(id) AS lo, max(id) AS hi FROM sage.%s)
		SELECT EXISTS (SELECT 1 FROM sage.snapshots d, ids
		    WHERE d.base_id BETWEEN ids.lo AND ids.hi AND d.collected_at >= $1
		      AND EXISTS (SELECT 1 FROM sage.%s b WHERE b.id = d.base_id))`,
		p.Name, p.Name), retainedFrom).Scan(&needed)
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

// truncateExpired empties the history partition at once when every row in
// it is older than cutoff (and nothing retained is built on them): one
// TRUNCATE instead of deleting gigabytes of pre-upgrade rows in batches.
func (c *Cleaner) truncateExpired(ctx context.Context, t partition.Table, p partition.Partition,
	cutoff time.Time, stats *RunStats) bool {
	var newest *time.Time
	if err := c.pool.QueryRow(ctx, fmt.Sprintf(`SELECT max(%s) FROM sage.%s`, t.Column,
		p.Name)).Scan(&newest); err != nil {
		c.logFn("WARN", "retention: read the newest row of sage.%s: %v", p.Name, err)
		return false
	}
	if newest == nil || !newest.Before(cutoff) || !c.removable(ctx, t, p, cutoff) {
		return newest == nil // empty: nothing to delete either
	}
	if err := partition.Truncate(ctx, c.pool, t, p); err != nil {
		c.logFn("WARN", "retention: truncate expired %s: %v (retrying next run)", p.Name, err)
		return false
	}
	stats.Truncated = append(stats.Truncated, p.Name)
	c.logFn("INFO", "retention: truncated expired partition sage.%s", p.Name)
	return true
}
