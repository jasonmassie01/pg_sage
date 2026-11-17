package executor

import (
	"context"
	"strconv"
)

// configKindColumnStatistics is a set_column_statistics change: its prior
// pg_attribute.attstattarget is captured before it runs, its rollback sets
// that target back, and it is read back after it ran (review-02c).
const configKindColumnStatistics = "column_statistics"

// prepareColumnStatistics captures the column's prior target and builds
// the rollback that restores it.
func (e *Executor) prepareColumnStatistics(
	ctx context.Context, c *configChange, stmt ColumnStatisticsStmt,
) error {
	prior, err := captureColumnStatTarget(ctx, e.pool, stmt)
	if err != nil {
		return err
	}
	rollback, err := columnStatisticsRollbackSQL(stmt, prior)
	if err != nil {
		return err
	}
	c.kind, c.colStats, c.priorStatTarget, c.rollbackSQL = configKindColumnStatistics, stmt,
		prior, rollback
	return nil
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
