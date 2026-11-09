package schema

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// Sequence forecast retirement (forecast-05). Two engines forecast the
// same sequences: the forecaster against each sequence's own range and
// the runway monitor against its binding limit (the owning column's type
// included). The runway is now the only one, so the forecaster's open
// findings would never resolve: every bootstrap resolves them, naming the
// successor in their detail. Resolved and other findings are untouched,
// so a run with nothing open changes nothing. A lock or statement
// timeout defers the step to the next bootstrap instead of failing it.

// retiredSequenceCategory is the retired forecaster rule's category.
const retiredSequenceCategory = "forecast_sequence_exhaustion"

// retiredSequenceReason is recorded as detail.resolved_reason.
const retiredSequenceReason = "superseded by forecast_sequence_runway"

// retireSequenceLockTimeout bounds the wait for a finding another session
// holds; the statement gets ten times as long.
var retireSequenceLockTimeout = 2 * time.Second

const retireSequenceSQL = `UPDATE sage.findings
   SET status = 'resolved', resolved_at = now(),
       detail = COALESCE(detail, '{}'::jsonb) ||
                jsonb_build_object('resolved_reason', $2::text)
 WHERE category = $1 AND status = 'open'`

// retireSequenceForecast resolves the open findings of the retired rule.
func retireSequenceForecast(ctx context.Context, db bootstrapDB) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("retire %s findings: begin: %w", retiredSequenceCategory, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lock := strconv.FormatInt(retireSequenceLockTimeout.Milliseconds(), 10) + "ms"
	stmt := strconv.FormatInt(10*retireSequenceLockTimeout.Milliseconds(), 10) + "ms"
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true),
		set_config('statement_timeout', $2, true)`, lock, stmt); err != nil {
		return fmt.Errorf("retire %s findings: set timeouts: %w", retiredSequenceCategory,
			err)
	}
	tag, err := tx.Exec(ctx, retireSequenceSQL, retiredSequenceCategory,
		retiredSequenceReason)
	if locksCleanupDeferrable(err) {
		slog.Warn("schema: resolving retired sequence-exhaustion findings deferred to "+
			"the next bootstrap", "error", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve open %s findings: %w", retiredSequenceCategory, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("retire %s findings: commit: %w", retiredSequenceCategory, err)
	}
	if n := tag.RowsAffected(); n > 0 {
		slog.Info("schema: resolved open sequence-exhaustion findings: "+
			retiredSequenceReason, "findings", n)
	}
	return nil
}
