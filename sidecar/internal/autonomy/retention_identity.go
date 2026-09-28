package autonomy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// retentionTarget is the identity a retention dry run is bound to (decision
// D5): the relation, the declared column's attnum and type, and the contract
// version. A rename swap, a rebuilt table or a re-declared contract changes
// it, so an aged dry run can never authorize deletion against a different
// column, relation or declaration. The delete transaction re-verifies it.
type retentionTarget struct {
	relationOID       uint32
	columnAttnum      int16
	columnType        string
	contractID        int64
	contractUpdatedAt time.Time
}

func (target retentionTarget) sameAs(other retentionTarget) bool {
	return target.relationOID == other.relationOID &&
		target.columnAttnum == other.columnAttnum &&
		target.columnType == other.columnType &&
		target.contractID == other.contractID &&
		target.contractUpdatedAt.Equal(other.contractUpdatedAt)
}

// retentionTargetSQL resolves the newest contract for the table and requires
// that it still declares $3 as a live, temporal column of a table.
const retentionTargetSQL = `SELECT tc.id, tc.updated_at, tbl.oid, att.attnum,
       typ.typname::text
FROM (
    SELECT id, updated_at, schema_name, table_name, append_only,
           retention_interval, retention_column
    FROM sage.table_contract WHERE schema_name=$1 AND table_name=$2
    ORDER BY updated_at DESC, id DESC LIMIT 1
) tc
JOIN pg_namespace ns ON ns.nspname=tc.schema_name
JOIN pg_class tbl ON tbl.relnamespace=ns.oid AND tbl.relname=tc.table_name
  AND tbl.relkind IN ('r','p')
JOIN pg_attribute att ON att.attrelid=tbl.oid AND att.attname=tc.retention_column
  AND att.attnum>0 AND NOT att.attisdropped
JOIN pg_type typ ON typ.oid=att.atttypid
WHERE tc.retention_column=$3 AND tc.append_only AND tc.retention_interval IS NOT NULL
  AND typ.typname IN ('timestamp','timestamptz','date')`

type retentionQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func resolveRetentionTarget(
	ctx context.Context, db retentionQuerier, invariant schemaguard.Invariant,
) (retentionTarget, error) {
	var target retentionTarget
	err := db.QueryRow(ctx, retentionTargetSQL, invariant.Schema, invariant.Table,
		invariant.RetentionColumn).Scan(&target.contractID, &target.contractUpdatedAt,
		&target.relationOID, &target.columnAttnum, &target.columnType)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, fmt.Errorf("%s.%s has no current retention contract declaring "+
			"temporal column %q; nothing deleted", invariant.Schema, invariant.Table,
			invariant.RetentionColumn)
	}
	if err != nil {
		return target, fmt.Errorf("resolve retention target: %w", err)
	}
	return target, nil
}

// Candidate drift: when the population now eligible is far larger than the
// reviewed dry run described, the review no longer covers what would be
// deleted, so a new dry run (and review window) is required.
const (
	retentionDriftFactor = 2
	retentionDriftSlack  = 100
)

func retentionCandidatesDrifted(reviewed, current int64) bool {
	return current > retentionDriftFactor*reviewed+retentionDriftSlack
}

type retentionDryRunState struct {
	pending, reviewed  int64
	reviewedCandidates int64
	// reviewUntil is when the oldest pending dry run leaves review, by the
	// database clock; zero when no dry run is pending.
	reviewUntil time.Time
}

// dryRunState counts dry runs recorded against exactly this target and
// window: those still inside the review window, those past it, and the
// candidate count of the newest reviewed one.
func (enforcer *postgresRetentionEnforcer) dryRunState(
	ctx context.Context, item schemaguard.Remediation, target retentionTarget,
) (retentionDryRunState, error) {
	var state retentionDryRunState
	err := enforcer.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE created_at > now() - make_interval(secs => $5)),
		count(*) FILTER (WHERE created_at <= now() - make_interval(secs => $5)),
		COALESCE((array_agg(candidate_rows ORDER BY created_at DESC)
			FILTER (WHERE created_at <= now() - make_interval(secs => $5)))[1], 0),
		COALESCE(min(created_at) FILTER (
			WHERE created_at > now() - make_interval(secs => $5)
		) + make_interval(secs => $5), 'epoch'::timestamptz)
		FROM sage.retention_run
		WHERE schema_name=$1 AND table_name=$2 AND retention_column=$3
		  AND disposition='dry_run'
		  AND abs(extract(epoch FROM (created_at - cutoff_at)) - $4) < $6
		  AND created_at > now() - make_interval(secs => $7)
		  AND relation_oid=$8 AND column_attnum=$9 AND column_type=$10
		  AND contract_id=$11 AND contract_updated_at=$12`,
		item.Invariant.Schema, item.Invariant.Table, item.Invariant.RetentionColumn,
		item.Contract.RetentionWindow.Seconds(), retentionDryRunMinAge.Seconds(),
		retentionWindowTolerance.Seconds(), retentionDryRunMaxAge.Seconds(),
		target.relationOID, target.columnAttnum, target.columnType,
		target.contractID, target.contractUpdatedAt,
	).Scan(&state.pending, &state.reviewed, &state.reviewedCandidates, &state.reviewUntil)
	if err != nil {
		return state, fmt.Errorf("read retention dry-run state: %w", err)
	}
	return state, nil
}

// verifyRetentionTargetInTx locks the table against DDL (ROW EXCLUSIVE
// conflicts with the ACCESS EXCLUSIVE a rename, drop or type change needs)
// and re-resolves the target in the same transaction as the DELETE, so the
// column cannot change identity between the check and the delete.
func verifyRetentionTargetInTx(
	ctx context.Context, tx pgx.Tx, invariant schemaguard.Invariant, want retentionTarget,
) error {
	_, err := tx.Exec(ctx, "LOCK TABLE "+qualifiedIdentifier(invariant)+
		" IN ROW EXCLUSIVE MODE")
	if err != nil {
		return fmt.Errorf("lock retention table: %w", err)
	}
	current, err := resolveRetentionTarget(ctx, tx, invariant)
	if err != nil {
		return err
	}
	if !current.sameAs(want) {
		return fmt.Errorf("%s.%s retention column %q or its contract changed identity "+
			"after the dry run was reviewed; nothing deleted", invariant.Schema,
			invariant.Table, invariant.RetentionColumn)
	}
	return nil
}
