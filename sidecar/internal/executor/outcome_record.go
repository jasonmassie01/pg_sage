package executor

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/value"
	"github.com/pg-sage/sidecar/internal/verify"
)

// recordPrediction stores an executed action's prediction as its pending
// outcome (sage.action_outcome).
func (e *Executor) recordPrediction(ctx context.Context, actionID int64, before map[string]any) {
	p, ok := predictionIn(before)
	if actionID <= 0 || e.pool == nil || !ok {
		return
	}
	if err := verify.NewOutcomeStore(e.pool).RecordPrediction(ctx, actionID, p); err != nil {
		e.logFn("executor", "record prediction for action %d: %v", actionID, err)
	}
}

// lifecycleOutcome is action_log.outcome for a kept action: success when
// the verifier measured it (improved or neutral), unverifiable when it
// could not tell.
func lifecycleOutcome(verdict string) string {
	switch verdict {
	case verify.OutcomeImproved, verify.OutcomeNeutral:
		return "success"
	}
	return "unverifiable"
}

// verificationVerdictFor is sage.verification.verdict for an outcome. Only
// an improvement is "success", the verdict value credit and earned
// autonomy count; a regression is a revert.
func verificationVerdictFor(verdict string) string {
	switch verdict {
	case verify.OutcomeImproved:
		return "success"
	case verify.OutcomeRegressed:
		return "revert"
	}
	return "unverifiable"
}

// settleOutcome finishes a verified action that is kept: its lifecycle
// outcome (only while still monitorable, so an operator rollback is never
// overwritten), the outcome ledger row, the verification verdict, and
// value credit for an improvement only. It reports whether it settled.
func settleOutcome(
	ctx context.Context, pool *pgxpool.Pool, o verify.Outcome,
	logFn func(string, string, ...any),
) bool {
	if !setMonitoredOutcome(ctx, pool, o.ActionLogID, lifecycleOutcome(o.Verdict), o.Reason) {
		logFn("verify", "action %d changed state; verdict %s not recorded", o.ActionLogID,
			o.Verdict)
		return false
	}
	logFn("verify", "action %d: %s (%s)", o.ActionLogID, o.Verdict, o.Reason)
	recordVerdict(ctx, pool, o, logFn)
	verificationID, err := finalizeActionVerification(ctx, pool, o.ActionLogID,
		verificationVerdictFor(o.Verdict), o.Reason)
	if err != nil {
		logFn("verify", "finalize verification of action %d: %v", o.ActionLogID, err)
	}
	if o.Verdict == verify.OutcomeImproved && verificationID > 0 {
		creditVerified(ctx, pool, o.ActionLogID, logFn)
	}
	return true
}

// recordVerdict writes the outcome ledger row.
func recordVerdict(
	ctx context.Context, pool *pgxpool.Pool, o verify.Outcome,
	logFn func(string, string, ...any),
) {
	if err := verify.NewOutcomeStore(pool).RecordVerdict(ctx, o); err != nil {
		logFn("verify", "record outcome of action %d: %v", o.ActionLogID, err)
	}
}

func creditVerified(
	ctx context.Context, pool *pgxpool.Pool, actionID int64,
	logFn func(string, string, ...any),
) {
	_, err := value.NewService(value.NewPostgresRepository(pool)).
		CreditVerifiedAction(ctx, actionID)
	if err != nil && !errors.Is(err, value.ErrToilModelUnavailable) {
		logFn("verify", "credit verified action %d: %v", actionID, err)
	}
}
