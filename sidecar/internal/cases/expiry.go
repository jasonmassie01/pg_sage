package cases

import (
	"strings"
	"time"
)

// Candidate restrictions the cases view sets from the recommendation
// lifecycle and from evidence age (cases-10).
const (
	// ReasonEvidenceExpired blocks a candidate whose evidence is past its
	// expires_at: it waits for a fresh observation.
	ReasonEvidenceExpired = "evidence_expired"
	// ReasonRecommendationReverted blocks content pg_sage applied and
	// reverted; the store never re-proposes it.
	ReasonRecommendationReverted = "recommendation_reverted"
	// ReasonRecommendationAbandoned blocks content whose retry budget is
	// exhausted; the store never re-proposes it.
	ReasonRecommendationAbandoned = "recommendation_abandoned"
)

// recommendationReasonPrefix marks the refusals a fresh observation of
// the same content cannot lift.
const recommendationReasonPrefix = "recommendation_"

// MarkExpiredCandidates blocks every candidate that IsExecutable(now)
// rejects with ReasonEvidenceExpired, replacing any earlier restriction
// except a recommendation refusal. A zero now marks nothing.
func MarkExpiredCandidates(list []Case, now time.Time) {
	if now.IsZero() {
		return
	}
	for i := range list {
		for j := range list[i].ActionCandidates {
			c := &list[i].ActionCandidates[j]
			if c.IsExecutable(now) ||
				strings.HasPrefix(c.BlockedReason, recommendationReasonPrefix) {
				continue
			}
			c.BlockedReason = ReasonEvidenceExpired
		}
	}
}
