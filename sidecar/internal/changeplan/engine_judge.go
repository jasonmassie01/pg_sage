package changeplan

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// judge reads judging step s's verdict and carries out its outcome
// action: keep, revert_step (the rollback runs after the commit, through
// the executor's RollbackAction) or halt_ask. A step the verifier already
// rolled back is recorded reverted and never rolled back again. A pending
// verdict before the deadline waits; a failed read waits too (logged).
func (e *Engine) judge(ctx context.Context, tx pgx.Tx, p Plan, s Step) (followUp, error) {
	if s.Runtime.ActionLogID == nil {
		return nil, e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StateJudging,
			StateHalted, stepWrite{reason: "the step has no action to judge"})
	}
	now := e.cfg.Now()
	v, err := e.cfg.Verdicts.Verdict(ctx, *s.Runtime.ActionLogID)
	if err != nil {
		e.cfg.LogFn("plan", "plan %d step %d: read the verdict: %v", p.ID, s.Ordinal, err)
		return nil, nil
	}
	o, byVerifier, done := judgeOutcome(s, v, now)
	if !done {
		return nil, nil
	}
	w := stepWrite{verdict: o, verdictAt: &now, claim: true,
		detail: map[string]any{"verifier_verdict": v.Verdict,
			"action_outcome": v.ActionOutcome}}
	if w.reason = unknownVerdict(v.Verdict); w.reason != "" {
		return nil, e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StateJudging,
			StateHalted, w)
	}
	if byVerifier {
		w.reason = ReasonRevertedByVerifier
		if v.Verdict == "" || v.Verdict == "pending" {
			w.reason = ReasonRevertedOutside
		}
		return nil, e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StateJudging,
			StateReverted, w)
	}
	switch act := s.ActionFor(o); act {
	case ActionKeep:
		return nil, e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StateJudging,
			StateKept, w)
	case ActionRevertStep:
		w.reason = "verdict " + string(o) + ": reverting the step"
		if err := e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StateJudging,
			StateReverting, w); err != nil {
			return nil, err
		}
		return func(ctx context.Context) error { return e.revert(ctx, p, s, o) }, nil
	default:
		// halt_ask, and revert_to_checkpoint until plan-02c: a person decides.
		w.reason = fmt.Sprintf("verdict %s asks a person (%s)", o, act)
		return nil, e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StateJudging,
			StateHalted, w)
	}
}

// verifierVerdicts are the stored verifier outcomes the plan maps
// (MapVerifierOutcome), plus "" and "pending" while undecided.
var verifierVerdicts = map[string]bool{"": true, "pending": true, "improved": true,
	"neutral": true, "regressed": true, "insufficient_evidence": true,
	"unverifiable": true, "expired": true}

// unknownVerdict is why a verifier verdict outside that vocabulary halts
// its step ("" for a known one): any other string halts the plan whatever
// the step's on_inconclusive says, even if the action was rolled back
// (plan-01a's rule, fail closed).
func unknownVerdict(verdict string) string {
	if verifierVerdicts[verdict] {
		return ""
	}
	return fmt.Sprintf("unknown verifier verdict %q: a person decides", truncate(verdict, 100))
}

// revert rolls step s's action back through the executor's RollbackAction
// (which routes an index replace to RollbackIndexReplace): reverted, or
// failed with the rollback's error.
func (e *Engine) revert(ctx context.Context, p Plan, s Step, o Outcome) error {
	reason := fmt.Sprintf("plan %d step %d: verdict %s (on_%s revert_step)", p.ID,
		s.Ordinal, o, o)
	if err := e.cfg.Executor.RollbackAction(ctx, *s.Runtime.ActionLogID,
		reason); err != nil {
		return e.transition(ctx, p.ID, s.ID, StateReverting, StateFailed,
			stepWrite{reason: "rollback: " + err.Error()})
	}
	return e.transition(ctx, p.ID, s.ID, StateReverting, StateReverted,
		stepWrite{reason: "reverted by the plan: verdict " + string(o)})
}
