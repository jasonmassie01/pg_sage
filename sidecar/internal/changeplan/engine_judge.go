package changeplan

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// judge reads judging step s's verdict and carries out its outcome
// action: keep, revert_step or revert_to_checkpoint (the reverts run after
// the commit, through RevertTo) or halt_ask. A step the verifier already
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
	case ActionRevertStep, ActionRevertToCheckpoint:
		w.reason = fmt.Sprintf("verdict %s: %s", o, act)
		if err := e.transitionTx(ctx, tx, p.ID, p.PlanHash, s.ID, StateJudging,
			StateReverting, w); err != nil {
			return nil, err
		}
		s.Runtime.State, s.Runtime.Verdict = StateReverting, o
		return func(ctx context.Context) error { return e.RevertTo(ctx, p, s, act) }, nil
	default:
		// halt_ask: a person decides.
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
