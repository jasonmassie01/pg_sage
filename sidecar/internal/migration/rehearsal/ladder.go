package rehearsal

import (
	"errors"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Ladder is the tier-2 record of a migration rehearsal (lab-04b): passed
// when it promotes the expand steps, failed when it parks them, and
// inconclusive when it stays a recommendation. A rehearsal that never had
// a clone (stale snapshot, provider outage, no receipt store) skips the
// tier with its reason. The record carries the
// clone receipt, the provider and how many affected statements were
// measured.
func Ladder(result Result) verify.LadderRecord {
	reason := string(result.Reason)
	if result.Detail != "" {
		reason += ": " + result.Detail
	}
	if result.CloneID == "" {
		if reason == "" {
			reason = "no rehearsal clone"
		}
		return verify.LadderBuildError(verify.TierClone, verify.MethodLadderRehearsal,
			errors.New(reason), result.At)
	}
	rec := verify.FromRehearsal(verify.Rehearsal{
		Passed:       result.Verdict == VerdictPromoteExpand && result.CleanupError == "",
		Inconclusive: result.Verdict == VerdictRecommendOnly,
		Reason:       reason, ReceiptID: result.ReceiptID, Provider: result.Provider,
	}, result.At)
	measured := 0
	for _, query := range result.Measurement.AffectedQueries {
		if _, ok := query.metric(); ok {
			measured++
		}
	}
	rec.Evidence["verdict"] = string(result.Verdict)
	rec.Evidence["measured"] = measured
	rec.Evidence["statements"] = len(result.Measurement.AffectedQueries)
	if result.CleanupError != "" {
		rec.Evidence["cleanup_error"] = result.CleanupError
	}
	return rec
}
