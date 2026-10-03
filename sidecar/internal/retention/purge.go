package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/partition"
)

// purgeTable purges sage.<table> rows older than retentionDays, with no
// run budget (a whole-table purge outside a run).
func (c *Cleaner) purgeTable(
	ctx context.Context,
	table string,
	timeCol string,
	retentionDays int,
	extraWhere string,
) {
	if retentionDays <= 0 {
		return
	}
	stats := newRunStats()
	c.purge(ctx, purgeRule{table: table, timeCol: timeCol, days: retentionDays,
		extra: extraWhere}, &stats, time.Now().Add(24*time.Hour))
}

// purge applies one rule. It returns false when the run's deadline passed
// before the rule was done (a later run resumes it), true otherwise
// (done, disabled, or failed and logged).
func (c *Cleaner) purge(ctx context.Context, rule purgeRule, stats *RunStats,
	deadline time.Time) bool {
	if rule.days <= 0 {
		return true
	}
	kind, err := c.relkind(ctx, rule.table)
	switch {
	case err != nil:
		c.logFn("ERROR", "retention: purging sage.%s failed: %v", rule.table, err)
		return true
	case kind == "" && rule.optional:
		return true // created on first use; nothing to purge yet
	case kind == "":
		c.logFn("ERROR", "retention: purging sage.%s failed: the table does not exist",
			rule.table)
		return true
	case kind == "p":
		return c.purgePartitioned(ctx, rule, stats, deadline)
	}
	return c.purgeRows(ctx, rule, "sage."+rule.table, stats, deadline)
}

// relkind is sage.<table>'s relkind, "" when it does not exist.
func (c *Cleaner) relkind(ctx context.Context, table string) (string, error) {
	var kind *string
	err := c.pool.QueryRow(ctx, `SELECT (SELECT relkind::text FROM pg_catalog.pg_class
		WHERE oid = pg_catalog.to_regclass($1))`, "sage."+table).Scan(&kind)
	if err != nil || kind == nil {
		return "", err
	}
	return *kind, nil
}

// purgeRows deletes rule's expired rows from relation (sage.<table>, or
// one partition of it) in paced batches until none remain or the deadline
// passes (paced).
func (c *Cleaner) purgeRows(ctx context.Context, rule purgeRule, relation string,
	stats *RunStats, deadline time.Time) bool {
	batch := rule.batch
	if batch <= 0 {
		batch = batchSize
	}
	query := purgeSQL(rule, relation, batch)
	total := int64(0)
	defer func() { c.logPurged(rule, relation, total) }()
	done, err := c.paced(ctx, batch, deadline, func() (int64, error) {
		tag, err := c.pool.Exec(ctx, query, rule.days)
		if err != nil {
			return 0, err
		}
		stats.count(rule.table, tag.RowsAffected())
		total += tag.RowsAffected()
		return tag.RowsAffected(), nil
	})
	if err != nil {
		c.logFn("ERROR", "retention: purging %s failed after %d rows: %v", relation,
			total, err)
	}
	return done
}

// paced runs step, one statement deleting at most batch rows, until a
// statement deletes fewer, ctx ends, or the deadline passes, pausing
// between statements. It returns false when the deadline cut it short
// (a later run resumes), true otherwise. A statement is never sent after
// the deadline, but the first one always is, so every run makes progress.
func (c *Cleaner) paced(ctx context.Context, batch int, deadline time.Time,
	step func() (int64, error)) (bool, error) {
	for ctx.Err() == nil {
		n, err := step()
		if err != nil {
			return true, err
		}
		if n < int64(batch) {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		if !c.sleep(ctx) {
			return true, nil
		}
	}
	return true, nil
}

func (c *Cleaner) logPurged(rule purgeRule, relation string, total int64) {
	if total > 0 {
		c.logFn("INFO", "retention: purged %d rows from %s (retention: %d days)",
			total, relation, rule.days)
	}
}

// sleep pauses between two statements; false when ctx ended.
func (c *Cleaner) sleep(ctx context.Context) bool {
	t := time.NewTimer(c.pause)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// partitionedTable is the day-partitioned table a rule purges.
func partitionedTable(rule purgeRule) (partition.Table, bool) {
	if rule.partitioned != nil {
		return *rule.partitioned, true
	}
	for _, t := range partition.HistoryTables() {
		if t.Name == rule.table {
			return t, true
		}
	}
	return partition.Table{}, false
}

// purgeSQL deletes one batch of rule's expired rows from relation. The
// relation is aliased by the table's name: keep predicates refer to the
// row being purged that way, also when it lives in a partition. A
// partition (history, default) is purged oldest first along its time
// index: most of it may be expired at once, and a LIMIT over a sequential
// scan reads the whole heap when nothing is left to delete. The batch's
// ctids are collected first (ARRAY), so the delete is a TID scan; with
// ctid IN (subquery) the planner may hash-join a full scan of the table.
func purgeSQL(rule purgeRule, relation string, batch int) string {
	order := ""
	if relation != "sage."+rule.table {
		order = "ORDER BY " + rule.timeCol
	}
	return fmt.Sprintf(`DELETE FROM %s WHERE ctid = ANY (ARRAY(
		SELECT ctid FROM %s AS %s
		WHERE %s < now() - make_interval(days => $1)
		%s
		%s LIMIT %d))`, relation, relation, pgx.Identifier{rule.table}.Sanitize(),
		rule.timeCol, rule.extra, order, batch)
}
