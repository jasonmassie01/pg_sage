package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/extstats"
)

// CREATE STATISTICS execution (owner decision 2026-10-04, PR #110). Only
// the pg_sage form parsed by extstats runs; its ANALYZE runs in the same
// transaction, and its rollback is the drop of exactly the created object.

// isPgSageCreateStatistics reports the CREATE STATISTICS form pg_sage runs.
func isPgSageCreateStatistics(sql string) bool {
	_, err := extstats.ParseCreate(sql)
	return err == nil
}

// isPgSageDropStatistics reports the DROP STATISTICS form pg_sage runs.
func isPgSageDropStatistics(sql string) bool {
	_, err := extstats.ParseDrop(sql)
	return err == nil
}

// checkStatisticsForm refuses any CREATE/DROP STATISTICS but the pg_sage
// form (normalized is the comment-free, whitespace-folded statement).
func checkStatisticsForm(normalized, prefix string) error {
	var err error
	switch prefix {
	case "CREATE STATISTICS":
		_, err = extstats.ParseCreate(normalized)
	case "DROP STATISTICS":
		_, err = extstats.ParseDrop(normalized)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDisallowedSQL, err)
	}
	return nil
}

// statisticsRollback is the rollback a CREATE STATISTICS runs with: the
// derived drop of the created object when none is given, the given one
// when it drops exactly that object, else ErrRollbackMismatch (a revert
// would drop someone else's statistics). Other statements keep theirs.
func statisticsRollback(sql, rollbackSQL string) (string, error) {
	c, err := extstats.ParseCreate(sql)
	if err != nil {
		return rollbackSQL, nil
	}
	if strings.TrimSpace(rollbackSQL) == "" {
		return c.Rollback(), nil
	}
	if !extstats.Undoes(c, rollbackSQL) {
		return "", fmt.Errorf("%w: %q does not undo %q", ErrRollbackMismatch, rollbackSQL, sql)
	}
	return rollbackSQL, nil
}

// prepareFindingRollback gives a statistics finding its rollback before it
// is queued or run.
func prepareFindingRollback(f *analyzer.Finding) error {
	rollback, err := statisticsRollback(f.RecommendedSQL, f.RollbackSQL)
	if err != nil {
		return err
	}
	f.RollbackSQL = rollback
	return nil
}

// ExecStatistics runs a pg_sage CREATE STATISTICS and the ANALYZE that
// builds it in one transaction under statement_timeout (per statement)
// and lock_timeout: a failure of either leaves nothing behind.
func ExecStatistics(
	ctx context.Context, pool *pgxpool.Pool, sql string, timeout time.Duration,
	opts ...DDLOption,
) error {
	if err := ValidateExecutorSQL(sql); err != nil {
		return fmt.Errorf("SQL validation: %w", err)
	}
	c, err := extstats.ParseCreate(sql)
	if err != nil {
		return fmt.Errorf("SQL validation: %w: %v", ErrDisallowedSQL, err)
	}
	analyze := c.Analyze()
	if err := ValidateExecutorSQL(analyze); err != nil {
		return fmt.Errorf("SQL validation: %w", err)
	}
	if err := execStatementsInTransaction(ctx, pool, timeout, applyDDLOpts(opts), sql,
		analyze); err != nil {
		return err
	}
	markAnalyzed(c.QualifiedTable())
	return nil
}
