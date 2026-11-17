package executor

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// configKindColumnStatistics is a set_column_statistics change: its prior
// pg_attribute.attstattarget is captured before it runs, its rollback sets
// that target back, and it is read back after it ran (review-02c).
const configKindColumnStatistics = "column_statistics"

// prepareColumnStatistics captures the column's prior target and builds
// the rollback that restores it. It has no outcome metric: the ANALYZE
// that uses the new target is its own step.
func (e *Executor) prepareColumnStatistics(
	ctx context.Context, sql string, stmt ColumnStatisticsStmt,
) (*configChange, error) {
	prior, err := captureColumnStatTarget(ctx, e.pool, stmt)
	if err != nil {
		return nil, err
	}
	rollback, err := columnStatisticsRollbackSQL(stmt, prior)
	if err != nil {
		return nil, err
	}
	if !stmt.Only {
		if err := checkDescendantTargets(ctx, e.pool, stmt, prior); err != nil {
			return nil, err
		}
	}
	return &configChange{sql: sql, kind: configKindColumnStatistics, colStats: stmt,
		priorStatTarget: prior, rollbackSQL: rollback}, nil
}

// recordColumnStatistics is the change's before_state entry.
func (c *configChange) recordColumnStatistics(change map[string]any) {
	change["table"], change["column"] = c.colStats.Table, c.colStats.Column
	change["requested"], change["prior"] = c.colStats.Target, statTargetText(c.priorStatTarget)
}

// readColumnStatistics reads the target back: in effect when
// attstattarget reads the requested value (-1 also reads as NULL on
// PostgreSQL 17+).
func (e *Executor) readColumnStatistics(
	ctx context.Context, c *configChange,
) (readbackState, string, map[string]any) {
	live, err := captureColumnStatTarget(ctx, e.pool, c.colStats)
	if err != nil {
		return readbackUnconfirmed, "statistics target read-back failed: " + err.Error(),
			map[string]any{"state": readbackUnconfirmed}
	}
	name := c.colStats.Table + "." + c.colStats.Column
	state, note := readbackInEffect, name+" statistics target "+
		strconv.Itoa(c.colStats.Target)+" in effect"
	if !statTargetMatches(c.colStats.Target, live) {
		state, note = readbackNotInEffect, name+" statistics target did not take effect "+
			"(pg_attribute.attstattarget shows "+statTargetText(live)+")"
	}
	return state, note, map[string]any{"state": state, "attstattarget": statTargetText(live)}
}

// statTargetText prints an attstattarget ("default" for NULL).
func statTargetText(v *int16) string {
	if v == nil {
		return "default"
	}
	return strconv.Itoa(int(*v))
}

// checkDescendantTargets refuses a change without ONLY when an inheritance
// child or partition of the table has a target other than the table's
// own: PostgreSQL sets the new target on every descendant, and the
// rollback, which restores the table's prior to all of them, could not
// give that child its own target back.
func checkDescendantTargets(
	ctx context.Context, pool *pgxpool.Pool, stmt ColumnStatisticsStmt, prior *int16,
) error {
	var differ int
	var first *string
	err := pool.QueryRow(ctx, `/* pg_sage executor */ WITH RECURSIVE kids(oid) AS (
		SELECT inhrelid FROM pg_inherits WHERE inhparent = to_regclass($1)
		UNION SELECT i.inhrelid FROM pg_inherits i JOIN kids k ON i.inhparent = k.oid)
		SELECT count(*)::int, min(a.attrelid::regclass::text)
		  FROM kids k JOIN pg_attribute a ON a.attrelid = k.oid AND a.attname = $2
		   AND NOT a.attisdropped
		 WHERE COALESCE(a.attstattarget::int, -1) <> COALESCE($3::int, -1)`,
		stmt.Table, stmt.Column, prior).Scan(&differ, &first)
	if err != nil {
		return fmt.Errorf("%w: statistics targets of the children of %s.%s: %v",
			ErrConfigPrior, stmt.Table, stmt.Column, err)
	}
	if differ > 0 {
		return fmt.Errorf("%w: %d inheritance children or partitions of %s (%s first) have "+
			"their own statistics target on %s; SET STATISTICS without ONLY would overwrite "+
			"it and the rollback could not restore it", ErrNoFaithfulRollback, differ,
			stmt.Table, *first, stmt.Column)
	}
	return nil
}
