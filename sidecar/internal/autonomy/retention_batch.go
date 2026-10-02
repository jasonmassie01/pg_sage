package autonomy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// ErrRetentionVerification reports a batch whose deleted rows do not match
// what it was authorized to delete; the batch rolls back.
var ErrRetentionVerification = errors.New("retention batch failed verification")

// deleteBatch deletes at most one bounded batch: the enforcer's limit, the
// reviewed dry run's remaining bound and the pipeline's row cap. Victims
// are bound to (tableoid, ctid) — a ctid alone repeats across partitions —
// and the cutoff is re-checked on the row actually deleted. Timeouts bound
// lock waits. The declared column's identity is re-verified under a table
// lock first, and the deleted rows are verified (count, declared predicate,
// contracted relation) before commit. The 'applied' audit row, linked to
// the pipeline's action and the reviewed dry run, is written in the same
// transaction: a deletion never exists without its durable record.
func (enforcer *postgresRetentionEnforcer) deleteBatch(
	ctx context.Context, plan retentionPlan, run RetentionRun,
) (RetentionResult, error) {
	maxRows, err := enforcer.batchRows(plan, run)
	if err != nil {
		return RetentionResult{}, err
	}
	tx, err := enforcer.pool.Begin(ctx)
	if err != nil {
		return RetentionResult{}, fmt.Errorf("begin retention batch: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := configureRetentionBatch(ctx, tx, run); err != nil {
		return RetentionResult{}, err
	}
	invariant := plan.item.Invariant
	if err := verifyRetentionTargetInTx(ctx, tx, invariant, plan.target); err != nil {
		return RetentionResult{}, err
	}
	result, err := deleteVerified(ctx, tx, plan, maxRows)
	if err != nil {
		return result, err
	}
	result.RunID, err = recordRetentionRun(ctx, tx, invariant, plan.target, plan.cutoff,
		plan.candidates, result.Deleted, "applied", run.ActionID, plan.dryRunID)
	if err != nil {
		return result, fmt.Errorf("retention batch rolled back: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return result, fmt.Errorf("commit retention batch: %w", err)
	}
	return result, nil
}

// batchRows is the batch's row cap. A plan without a reviewed bound
// deletes nothing.
func (enforcer *postgresRetentionEnforcer) batchRows(
	plan retentionPlan, run RetentionRun,
) (int64, error) {
	if plan.bound <= 0 {
		return 0, errors.New("retention batch has no reviewed dry-run bound; nothing deleted")
	}
	rows := min(int64(enforcer.limit()), plan.bound)
	if run.MaxRows > 0 {
		rows = min(rows, run.MaxRows)
	}
	return rows, nil
}

func configureRetentionBatch(ctx context.Context, tx pgx.Tx, run RetentionRun) error {
	lockTimeout := retentionLockTimeout
	if run.LockTimeout > 0 && run.LockTimeout < lockTimeout {
		lockTimeout = run.LockTimeout
	}
	for setting, value := range map[string]time.Duration{
		"statement_timeout": retentionStatementTimeout, "lock_timeout": lockTimeout,
	} {
		if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", setting,
			strconv.FormatInt(value.Milliseconds(), 10)+"ms"); err != nil {
			return fmt.Errorf("configure retention batch: %w", err)
		}
	}
	return nil
}

// deleteVerified runs the delete and verifies, in the same transaction,
// every row it deleted.
func deleteVerified(ctx context.Context, tx pgx.Tx, plan retentionPlan, maxRows int64,
) (RetentionResult, error) {
	invariant := plan.item.Invariant
	query := retentionDeleteSQL(qualifiedIdentifier(invariant),
		quoteIdentifier(invariant.RetentionColumn))
	var result RetentionResult
	if err := tx.QueryRow(ctx, query, plan.cutoff, maxRows, plan.target.relationOID).Scan(
		&result.Deleted, &result.OutsidePredicate, &result.OutsideRelation); err != nil {
		return result, fmt.Errorf("apply bounded retention batch: %w", err)
	}
	if err := verifyRetentionBatch(result, maxRows); err != nil {
		return result, fmt.Errorf("retention batch rolled back: %w", err)
	}
	return result, nil
}

// retentionDeleteSQL deletes one batch and reports, from the deleted rows
// themselves, how many there were, how many do not satisfy the declared
// predicate and how many belong neither to the contracted relation nor to
// one of its partitions (pg_partition_tree lists none for a plain table).
func retentionDeleteSQL(table, column string) string {
	return "WITH victims AS (SELECT tableoid, ctid FROM " + table +
		" WHERE " + column + " < $1 ORDER BY " + column +
		" LIMIT $2 FOR UPDATE SKIP LOCKED), deleted AS (" +
		"DELETE FROM " + table + " AS target USING victims " +
		"WHERE target.tableoid = victims.tableoid AND target.ctid = victims.ctid " +
		"AND target." + column + " < $1 " +
		"RETURNING target.tableoid AS rel, target." + column + " AS ts) " +
		"SELECT count(*), count(*) FILTER (WHERE ts IS NULL OR NOT (ts < $1)), " +
		"count(*) FILTER (WHERE rel <> ALL (ARRAY[$3::oid] || ARRAY(" +
		"SELECT relid::oid FROM pg_partition_tree($3::oid::regclass)))) FROM deleted"
}

// verifyRetentionBatch accepts a batch only when it deleted no more than
// its cap and every deleted row satisfied the declared predicate and
// belonged to the contracted relation.
func verifyRetentionBatch(result RetentionResult, maxRows int64) error {
	switch {
	case maxRows <= 0:
		return fmt.Errorf("%w: no row cap", ErrRetentionVerification)
	case result.Deleted < 0 || result.Deleted > maxRows:
		return fmt.Errorf("%w: deleted %d rows, cap %d", ErrRetentionVerification,
			result.Deleted, maxRows)
	case result.OutsidePredicate != 0:
		return fmt.Errorf("%w: %d deleted rows were not older than the cutoff",
			ErrRetentionVerification, result.OutsidePredicate)
	case result.OutsideRelation != 0:
		return fmt.Errorf("%w: %d deleted rows were outside the contracted relation",
			ErrRetentionVerification, result.OutsideRelation)
	}
	return nil
}

func (enforcer *postgresRetentionEnforcer) record(
	ctx context.Context, invariant schemaguard.Invariant, target retentionTarget,
	cutoff time.Time, candidates int64,
) error {
	_, err := recordRetentionRun(ctx, enforcer.pool, invariant, target, cutoff,
		candidates, 0, "dry_run", 0, 0)
	return err
}

// recordRetentionRun writes one sage.retention_run row; an applied batch
// links the action that ran it and the dry run that authorized it.
func recordRetentionRun(
	ctx context.Context, db retentionQuerier, invariant schemaguard.Invariant,
	target retentionTarget, cutoff time.Time, candidates, deleted int64,
	disposition string, actionID, dryRunID int64,
) (int64, error) {
	var id int64
	err := db.QueryRow(ctx, `INSERT INTO sage.retention_run
		(schema_name, table_name, retention_column, cutoff_at,
		 candidate_rows, deleted_rows, disposition, relation_oid, column_attnum,
		 column_type, contract_id, contract_updated_at, action_id, dry_run_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13::bigint,0),
		        NULLIF($14::bigint,0))
		RETURNING id`, invariant.Schema,
		invariant.Table, invariant.RetentionColumn, cutoff, candidates, deleted,
		disposition, target.relationOID, target.columnAttnum, target.columnType,
		target.contractID, target.contractUpdatedAt, actionID, dryRunID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("record retention enforcement: %w", err)
	}
	return id, nil
}
