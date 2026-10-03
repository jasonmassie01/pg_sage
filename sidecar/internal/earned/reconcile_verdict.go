package earned

import "github.com/pg-sage/sidecar/internal/verify"

// classifyWithVerdict maps an executed action to a ledger result, using
// its predicted-vs-observed verdict (sage.action_outcome, Phase 1.3) when
// it has one: only an improvement is a verified recovery, a regression is
// harmful, and neutral, insufficient evidence and unverifiable earn
// nothing and cost nothing. A failed action is not recovered whatever its
// verdict. Without a verdict the P0-6 rules of classifyOutcome apply.
func classifyWithVerdict(outcome, verification, verdict string, settled bool) (string, bool) {
	if outcome == "failed" || verdict == "" {
		return classifyOutcome(outcome, verification, settled)
	}
	switch verdict {
	case verify.OutcomeImproved:
		return ResultVerifiedRecovery, true
	case verify.OutcomeRegressed:
		return ResultHarmful, true
	case verify.OutcomeNeutral, verify.OutcomeInsufficient, verify.OutcomeUnverifiable:
		return ResultUnverified, true
	}
	return "", false
}
