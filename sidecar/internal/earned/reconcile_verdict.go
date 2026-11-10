package earned

import "github.com/pg-sage/sidecar/internal/verify"

// classifyWithVerdict maps an executed action to a ledger result, using
// its predicted-vs-observed verdict (sage.action_outcome, Phase 1.3) when
// it has one: only an improvement is a verified recovery, a regression is
// harmful, and neutral, insufficient evidence and unverifiable earn
// nothing and cost nothing. A failed action is not recovered whatever its
// verdict. Without a verdict the P0-6 rules of classifyOutcome apply.
//
// A rolled-back (or reverted) action earns nothing whatever its verdict
// but a regression: the rollback itself is judged by the rollback pass
// (earned-01), so the result does not depend on whether the rollback
// landed before this action was first read. A failed rollback is harmful.
func classifyWithVerdict(outcome, verification, verdict string, settled bool) (string, bool) {
	if outcome == "failed" || verdict == "" {
		return classifyOutcome(outcome, verification, settled)
	}
	if outcome == "rollback_failed" {
		return ResultHarmful, true
	}
	switch verdict {
	case verify.OutcomeImproved:
		if outcome == "rolled_back" || outcome == "reverted" {
			return ResultUnverified, true
		}
		return ResultVerifiedRecovery, true
	case verify.OutcomeRegressed:
		return ResultHarmful, true
	case verify.OutcomeNeutral, verify.OutcomeInsufficient, verify.OutcomeUnverifiable:
		return ResultUnverified, true
	}
	return "", false
}
