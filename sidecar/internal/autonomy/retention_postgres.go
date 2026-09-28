package autonomy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// ErrRetentionDryRunPending means deletion is waiting for a dry run that
// matches the current retention semantics and has passed its review window.
var ErrRetentionDryRunPending = errors.New("retention dry run pending review")

const (
	// A dry run authorizes deletion only after it could be reviewed, and
	// only while it is recent enough to describe the current population.
	retentionDryRunMinAge = 24 * time.Hour
	retentionDryRunMaxAge = 7 * 24 * time.Hour
	// Clock skew allowed between the recorded cutoff and the dry-run time.
	retentionWindowTolerance  = 5 * time.Minute
	retentionStatementTimeout = "30s"
	retentionLockTimeout      = "2s"
)

// RetentionIntent is the typed destructive action submitted to the policy
// gate immediately before each delete batch.
type RetentionIntent struct {
	Schema     string
	Table      string
	Column     string
	Cutoff     time.Time
	Window     time.Duration
	BatchLimit int
	Candidates int64
}

// RetentionAuthorizer returns nil only when the standing policy gate grants
// execution (emergency stop, executor enabled, trust, mode, windows,
// replica and change-class checks all pass).
type RetentionAuthorizer func(context.Context, RetentionIntent) error

type postgresRetentionEnforcer struct {
	pool       *pgxpool.Pool
	batchLimit int
	now        func() time.Time
	authorize  RetentionAuthorizer
}

func (enforcer *postgresRetentionEnforcer) Apply(
	ctx context.Context, item schemaguard.Remediation,
) error {
	if enforcer == nil || enforcer.pool == nil {
		return errors.New("retention enforcement database is unavailable")
	}
	if item.Contract.RetentionWindow <= 0 || item.Invariant.RetentionColumn == "" {
		return errors.New("retention enforcement requires a declared time column and window")
	}
	target, err := resolveRetentionTarget(ctx, enforcer.pool, item.Invariant)
	if err != nil {
		return err
	}
	cutoff := enforcer.currentTime().Add(-item.Contract.RetentionWindow)
	candidates, err := enforcer.candidateCount(ctx, item.Invariant, cutoff)
	if err != nil {
		return err
	}
	if item.Decision.Disposition == schemaguard.DispositionDryRun {
		return enforcer.record(ctx, item.Invariant, target, cutoff, candidates, 0, "dry_run")
	}
	if item.Decision.Disposition != schemaguard.DispositionApply {
		return errors.New("retention enforcement disposition is not actionable")
	}
	if err := enforcer.requireReviewedDryRun(ctx, item, target, cutoff,
		candidates); err != nil {
		return err
	}
	if err := enforcer.requirePolicyConsent(ctx); err != nil {
		return err
	}
	return enforcer.authorizedDelete(ctx, item, target, cutoff, candidates)
}

func (enforcer *postgresRetentionEnforcer) authorizedDelete(
	ctx context.Context, item schemaguard.Remediation, target retentionTarget,
	cutoff time.Time, candidates int64,
) error {
	if enforcer.authorize == nil {
		return errors.New("retention authorization is unavailable; nothing deleted")
	}
	intent := RetentionIntent{
		Schema: item.Invariant.Schema, Table: item.Invariant.Table,
		Column: item.Invariant.RetentionColumn, Cutoff: cutoff,
		Window: item.Contract.RetentionWindow, BatchLimit: enforcer.limit(),
		Candidates: candidates,
	}
	if err := enforcer.authorize(ctx, intent); err != nil {
		return fmt.Errorf("retention delete withheld: %w", err)
	}
	_, err := enforcer.deleteBatch(ctx, item.Invariant, target, cutoff, candidates)
	return err
}

func (enforcer *postgresRetentionEnforcer) requirePolicyConsent(
	ctx context.Context,
) error {
	current, err := policy.NewStore(enforcer.pool).Current(ctx, policy.Scope{})
	if err != nil {
		return fmt.Errorf("read retention policy consent: %w", err)
	}
	document, err := policy.ParseDocument(current.Document)
	if err != nil {
		return fmt.Errorf("parse retention policy consent: %w", err)
	}
	for _, class := range document.AllowedChangeClasses {
		if class == policy.ChangeRetention {
			return nil
		}
	}
	return errors.New("standing policy does not consent to retention enforcement")
}

func (enforcer *postgresRetentionEnforcer) candidateCount(
	ctx context.Context, invariant schemaguard.Invariant, cutoff time.Time,
) (int64, error) {
	query := "SELECT count(*) FROM " + qualifiedIdentifier(invariant) +
		" WHERE " + quoteIdentifier(invariant.RetentionColumn) + " < $1"
	var count int64
	if err := enforcer.pool.QueryRow(ctx, query, cutoff).Scan(&count); err != nil {
		return 0, fmt.Errorf("count retention candidates: %w", err)
	}
	return count, nil
}

// requireReviewedDryRun demands a dry run for the same relation, declared
// column identity, contract version and window that is at least
// retentionDryRunMinAge old and whose candidate count still describes the
// eligible population. When none qualifies and no dry run is already under
// review, a fresh one is recorded so the review clock starts; deletion never
// proceeds on stale, drifted or foreign evidence. Waiting for review is not
// a failure: it returns a schemaguard.ParkedRoute naming when review ends.
func (enforcer *postgresRetentionEnforcer) requireReviewedDryRun(
	ctx context.Context, item schemaguard.Remediation, target retentionTarget,
	cutoff time.Time, candidates int64,
) error {
	state, err := enforcer.dryRunState(ctx, item, target)
	if err != nil {
		return err
	}
	drifted := state.reviewed > 0 &&
		retentionCandidatesDrifted(state.reviewedCandidates, candidates)
	if state.reviewed > 0 && !drifted {
		return nil
	}
	if state.pending == 0 {
		if err := enforcer.record(ctx, item.Invariant, target, cutoff, candidates, 0,
			"dry_run"); err != nil {
			return err
		}
		if state, err = enforcer.dryRunState(ctx, item, target); err != nil {
			return err
		}
	}
	return &schemaguard.ParkedRoute{
		Reason: "retention dry run in review until " +
			state.reviewUntil.UTC().Format(time.RFC3339),
		Err: retentionPendingCause(item, candidates, state, drifted),
	}
}

func retentionPendingCause(item schemaguard.Remediation, candidates int64,
	state retentionDryRunState, drifted bool,
) error {
	if drifted {
		return fmt.Errorf("%w: %s.%s has %d retention candidates but its reviewed dry "+
			"run described %d; a new dry run must pass review", ErrRetentionDryRunPending,
			item.Invariant.Schema, item.Invariant.Table, candidates,
			state.reviewedCandidates)
	}
	return fmt.Errorf("%w: %s.%s needs a dry run for column %s and window %s "+
		"at least %s old", ErrRetentionDryRunPending, item.Invariant.Schema,
		item.Invariant.Table, item.Invariant.RetentionColumn,
		item.Contract.RetentionWindow, retentionDryRunMinAge)
}

// deleteBatch deletes at most one bounded batch. Victims are bound to
// (tableoid, ctid) — a ctid alone repeats across partitions — and the cutoff
// is re-checked on the row actually deleted. Timeouts bound lock waits.
// The declared column's identity is re-verified under a table lock first.
// The 'applied' audit row is written in the same transaction as the delete:
// either both commit or neither does, so a deletion can never exist without
// its durable outcome record (and an ambiguous commit leaves no half state).
func (enforcer *postgresRetentionEnforcer) deleteBatch(
	ctx context.Context, invariant schemaguard.Invariant, target retentionTarget,
	cutoff time.Time, candidates int64,
) (int64, error) {
	tx, err := enforcer.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin retention batch: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, setting := range []string{
		"SET LOCAL statement_timeout = '" + retentionStatementTimeout + "'",
		"SET LOCAL lock_timeout = '" + retentionLockTimeout + "'",
	} {
		if _, err := tx.Exec(ctx, setting); err != nil {
			return 0, fmt.Errorf("configure retention batch: %w", err)
		}
	}
	if err := verifyRetentionTargetInTx(ctx, tx, invariant, target); err != nil {
		return 0, err
	}
	query := retentionDeleteSQL(qualifiedIdentifier(invariant),
		quoteIdentifier(invariant.RetentionColumn))
	tag, err := tx.Exec(ctx, query, cutoff, enforcer.limit())
	if err != nil {
		return 0, fmt.Errorf("apply bounded retention batch: %w", err)
	}
	if err := recordRetentionRun(ctx, tx, invariant, target, cutoff, candidates,
		tag.RowsAffected(), "applied"); err != nil {
		return 0, fmt.Errorf("retention batch rolled back: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit retention batch: %w", err)
	}
	return tag.RowsAffected(), nil
}

func retentionDeleteSQL(table, column string) string {
	return "WITH victims AS (SELECT tableoid, ctid FROM " + table +
		" WHERE " + column + " < $1 ORDER BY " + column +
		" LIMIT $2 FOR UPDATE SKIP LOCKED) " +
		"DELETE FROM " + table + " AS target USING victims " +
		"WHERE target.tableoid = victims.tableoid AND target.ctid = victims.ctid " +
		"AND target." + column + " < $1"
}

func (enforcer *postgresRetentionEnforcer) limit() int {
	if enforcer.batchLimit <= 0 || enforcer.batchLimit > 1000 {
		return 1000
	}
	return enforcer.batchLimit
}

func (enforcer *postgresRetentionEnforcer) record(
	ctx context.Context, invariant schemaguard.Invariant, target retentionTarget,
	cutoff time.Time, candidates, deleted int64, disposition string,
) error {
	return recordRetentionRun(ctx, enforcer.pool, invariant, target, cutoff,
		candidates, deleted, disposition)
}

type retentionExecer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func recordRetentionRun(
	ctx context.Context, db retentionExecer, invariant schemaguard.Invariant,
	target retentionTarget, cutoff time.Time, candidates, deleted int64,
	disposition string,
) error {
	_, err := db.Exec(ctx, `INSERT INTO sage.retention_run
		(schema_name, table_name, retention_column, cutoff_at,
		 candidate_rows, deleted_rows, disposition, relation_oid, column_attnum,
		 column_type, contract_id, contract_updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, invariant.Schema,
		invariant.Table, invariant.RetentionColumn, cutoff, candidates, deleted,
		disposition, target.relationOID, target.columnAttnum, target.columnType,
		target.contractID, target.contractUpdatedAt)
	if err != nil {
		return fmt.Errorf("record retention enforcement: %w", err)
	}
	return nil
}

func (enforcer *postgresRetentionEnforcer) currentTime() time.Time {
	if enforcer.now != nil {
		return enforcer.now()
	}
	return time.Now()
}

func qualifiedIdentifier(invariant schemaguard.Invariant) string {
	return quoteIdentifier(invariant.Schema) + "." + quoteIdentifier(invariant.Table)
}
