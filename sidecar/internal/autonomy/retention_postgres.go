package autonomy

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	retentionStatementTimeout = 30 * time.Second
	// retentionLockTimeout bounds a batch's lock waits; the pipeline's
	// decision may lower it (the policy lock ceiling), never raise it.
	retentionLockTimeout = 2 * time.Second
)

// RetentionIntent is the typed destructive action submitted to the
// executor's pipeline for each delete batch.
type RetentionIntent struct {
	Schema string
	Table  string
	Column string
	// DeclaredColumn is the contract's owner-declared retention column
	// (sage.table_contract.retention_column); empty for a pre-D5 contract.
	DeclaredColumn string
	Cutoff         time.Time
	Window         time.Duration
	BatchLimit     int
	Candidates     int64
	// Bound is how many more rows the reviewed dry run authorizes, and
	// DryRunID that dry run (sage.retention_run).
	Bound    int64
	DryRunID int64
}

// RetentionRun is what the pipeline gives a batch: the action it runs
// under, the lock timeout of its authorized decision and its row cap
// (zero values keep the batch's own defaults).
type RetentionRun struct {
	ActionID    int64
	LockTimeout time.Duration
	MaxRows     int64
}

// RetentionResult is a committed batch: its sage.retention_run row and the
// rows it deleted, with how many fell outside the declared predicate or
// the contracted relation (a batch with any such row rolls back).
type RetentionResult struct {
	RunID            int64
	Deleted          int64
	OutsidePredicate int64
	OutsideRelation  int64
}

// RetentionBatch deletes one bounded, verified batch.
type RetentionBatch func(context.Context, RetentionRun) (RetentionResult, error)

// RetentionPipeline runs a batch through the executor's single execution
// pipeline (Executor.Apply): authorize, typed-target lease on the table,
// slot, re-authorize, run the batch as a recorded action, verify. It
// returns nil only when the batch ran and verified.
type RetentionPipeline func(context.Context, RetentionIntent, RetentionBatch) error

type postgresRetentionEnforcer struct {
	pool       *pgxpool.Pool
	batchLimit int
	now        func() time.Time
	pipeline   RetentionPipeline
}

// retentionPlan is one authorized-to-try batch: the remediation, the
// identity its reviewed dry run is bound to, and what that dry run still
// authorizes.
type retentionPlan struct {
	item       schemaguard.Remediation
	target     retentionTarget
	cutoff     time.Time
	candidates int64
	dryRunID   int64
	bound      int64
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
		return enforcer.record(ctx, item.Invariant, target, cutoff, candidates)
	}
	if item.Decision.Disposition != schemaguard.DispositionApply {
		return errors.New("retention enforcement disposition is not actionable")
	}
	review, err := enforcer.requireReviewedDryRun(ctx, item, target, cutoff, candidates)
	if err != nil {
		return err
	}
	if err := enforcer.requirePolicyConsent(ctx); err != nil {
		return err
	}
	return enforcer.authorizedDelete(ctx, retentionPlan{item: item, target: target,
		cutoff: cutoff, candidates: candidates, dryRunID: review.dryRunID,
		bound: review.bound})
}

// authorizedDelete hands the batch to the executor's pipeline. A table
// leased by another action parks the delete (it retries next tick).
func (enforcer *postgresRetentionEnforcer) authorizedDelete(
	ctx context.Context, plan retentionPlan,
) error {
	if enforcer.pipeline == nil {
		return errors.New("retention pipeline is unavailable; nothing deleted")
	}
	invariant, contract := plan.item.Invariant, plan.item.Contract
	intent := RetentionIntent{
		Schema: invariant.Schema, Table: invariant.Table,
		Column: invariant.RetentionColumn, Cutoff: plan.cutoff,
		DeclaredColumn: contract.RetentionColumn, Window: contract.RetentionWindow,
		BatchLimit: enforcer.limit(), Candidates: plan.candidates,
		Bound: plan.bound, DryRunID: plan.dryRunID,
	}
	batch := func(ctx context.Context, run RetentionRun) (RetentionResult, error) {
		return enforcer.deleteBatch(ctx, plan, run)
	}
	err := enforcer.pipeline(ctx, intent, batch)
	if errors.Is(err, policy.ErrLeaseConflict) || errors.Is(err, policy.ErrLeaseBusy) {
		return &schemaguard.ParkedRoute{Reason: "retention table is being changed by " +
			"another action", Err: err}
	}
	if err != nil {
		return fmt.Errorf("retention delete withheld: %w", err)
	}
	return nil
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

func (enforcer *postgresRetentionEnforcer) limit() int {
	if enforcer.batchLimit <= 0 || enforcer.batchLimit > 1000 {
		return 1000
	}
	return enforcer.batchLimit
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
