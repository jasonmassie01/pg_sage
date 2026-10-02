package autonomy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// retentionReview is the reviewed dry run that authorizes deletion and how
// many more rows it allows.
type retentionReview struct {
	dryRunID int64
	bound    int64
}

// requireReviewedDryRun demands a dry run for the same relation, declared
// column identity, contract version and window that is at least
// retentionDryRunMinAge old, whose candidate count still describes the
// eligible population, and whose bound is not used up. The bound is the
// drift ceiling of the population it described, less what batches under
// it already deleted: deletion never exceeds what the review covered.
// When none qualifies and no dry run is already under review, a fresh one
// is recorded so the review clock starts. Waiting for review is not a
// failure: it returns a schemaguard.ParkedRoute naming when review ends.
func (enforcer *postgresRetentionEnforcer) requireReviewedDryRun(
	ctx context.Context, item schemaguard.Remediation, target retentionTarget,
	cutoff time.Time, candidates int64,
) (retentionReview, error) {
	state, err := enforcer.dryRunState(ctx, item, target)
	if err != nil {
		return retentionReview{}, err
	}
	drifted := state.reviewed > 0 &&
		retentionCandidatesDrifted(state.reviewedCandidates, candidates)
	exhausted := false
	if state.reviewed > 0 && !drifted {
		review, err := enforcer.reviewedBound(ctx, state)
		if err != nil || review.bound > 0 {
			return review, err
		}
		exhausted = true
	}
	if state.pending == 0 {
		if err := enforcer.record(ctx, item.Invariant, target, cutoff,
			candidates); err != nil {
			return retentionReview{}, err
		}
		if state, err = enforcer.dryRunState(ctx, item, target); err != nil {
			return retentionReview{}, err
		}
	}
	return retentionReview{}, &schemaguard.ParkedRoute{
		Reason: "retention dry run in review until " +
			state.reviewUntil.UTC().Format(time.RFC3339),
		Err: retentionPendingCause(item, candidates, state, drifted, exhausted),
	}
}

// reviewedBound is what the newest reviewed dry run still authorizes.
func (enforcer *postgresRetentionEnforcer) reviewedBound(
	ctx context.Context, state retentionDryRunState,
) (retentionReview, error) {
	if state.reviewedID <= 0 {
		return retentionReview{}, errors.New("reviewed retention dry run has no identity")
	}
	var used int64
	if err := enforcer.pool.QueryRow(ctx, `SELECT COALESCE(sum(deleted_rows), 0)::bigint
		FROM sage.retention_run WHERE dry_run_id=$1 AND disposition='applied'`,
		state.reviewedID).Scan(&used); err != nil {
		return retentionReview{}, fmt.Errorf("read rows deleted under dry run %d: %w",
			state.reviewedID, err)
	}
	ceiling := retentionDriftFactor*state.reviewedCandidates + retentionDriftSlack
	return retentionReview{dryRunID: state.reviewedID, bound: ceiling - used}, nil
}

func retentionPendingCause(item schemaguard.Remediation, candidates int64,
	state retentionDryRunState, drifted, exhausted bool,
) error {
	table := item.Invariant.Schema + "." + item.Invariant.Table
	switch {
	case drifted:
		return fmt.Errorf("%w: %s has %d retention candidates but its reviewed dry "+
			"run described %d; a new dry run must pass review", ErrRetentionDryRunPending,
			table, candidates, state.reviewedCandidates)
	case exhausted:
		return fmt.Errorf("%w: %s used up the rows its reviewed dry run %d authorized; "+
			"a new dry run must pass review", ErrRetentionDryRunPending, table,
			state.reviewedID)
	}
	return fmt.Errorf("%w: %s needs a dry run for column %s and window %s "+
		"at least %s old", ErrRetentionDryRunPending, table,
		item.Invariant.RetentionColumn, item.Contract.RetentionWindow,
		retentionDryRunMinAge)
}
