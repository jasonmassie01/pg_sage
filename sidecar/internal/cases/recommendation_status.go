package cases

import (
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/recommendation"
)

// CaseRecommendation is the recommendation lifecycle behind a finding
// case (cases-10): the newest head of its finding, what happened to it and
// what pg_sage does next. URL is the head's read API.
type CaseRecommendation struct {
	ID            int64      `json:"id"`
	State         string     `json:"state"`
	Revision      int        `json:"revision"`
	Verdict       string     `json:"verdict,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	AttemptCount  int        `json:"attempt_count"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	AppliedAt     *time.Time `json:"applied_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
	// CurrentContent says the head carries the content the case proposes
	// now (the same content hash). A head of other content never blocks
	// the current candidate.
	CurrentContent bool   `json:"current_content"`
	NextStep       string `json:"next_step"`
	URL            string `json:"url"`
}

// AttachRecommendation links c to its finding's newest head. When the
// head carries the candidate's content, a reverted or abandoned head
// blocks the candidate and a failed head in backoff sets retrying_after.
// The head itself is never changed.
func AttachRecommendation(
	c *Case, head recommendation.FindingHead, url string, now time.Time,
) {
	if c == nil {
		return
	}
	src := caseSourceFinding(*c)
	current := src != nil && findingContentHash(head, *src) == head.ContentHash
	r := &CaseRecommendation{
		ID: head.ID, State: string(head.State), Revision: head.Revision,
		Verdict: head.Verdict, Reason: head.Reason, AttemptCount: head.AttemptCount,
		NextAttemptAt: head.NextAttemptAt, AppliedAt: head.AppliedAt,
		UpdatedAt: head.UpdatedAt, CurrentContent: current, URL: url,
	}
	r.NextStep = recommendationNextStep(*r, src != nil && !current, now)
	c.Recommendation, c.RecommendationUnknown = r, false
	if current {
		applyHeadToCandidates(c, *src, head, now)
	}
}

// MarkRecommendationUnknown records that c's recommendation heads could
// not be read, so its missing recommendation is not read as "none".
func MarkRecommendationUnknown(c *Case) {
	if c != nil {
		c.Recommendation, c.RecommendationUnknown = nil, true
	}
}

func caseSourceFinding(c Case) *SourceFinding {
	for _, cand := range c.ActionCandidates {
		if cand.Source != nil {
			return cand.Source
		}
	}
	return nil
}

// findingContentHash is the content hash the analyzer records for f's
// recommended change in the head's database.
func findingContentHash(head recommendation.FindingHead, f SourceFinding) string {
	return recommendation.ContentHash(recommendation.Proposal{
		DatabaseName: head.DatabaseName, Category: f.Category, Target: f.ObjectIdentifier,
		ForwardSQL: f.RecommendedSQL, InverseSQL: f.RollbackSQL,
	})
}

// applyHeadToCandidates marks the candidates that run f's recommended SQL.
func applyHeadToCandidates(
	c *Case, f SourceFinding, head recommendation.FindingHead, now time.Time,
) {
	forward := strings.TrimSpace(f.RecommendedSQL)
	for i := range c.ActionCandidates {
		cand := &c.ActionCandidates[i]
		if strings.TrimSpace(cand.ProposedSQL) != forward {
			continue
		}
		switch head.State {
		case recommendation.StateReverted:
			cand.BlockedReason = ReasonRecommendationReverted
		case recommendation.StateAbandoned:
			cand.BlockedReason = ReasonRecommendationAbandoned
		case recommendation.StateFailed:
			if retryPending(head.NextAttemptAt, now) {
				next := *head.NextAttemptAt
				cand.RetryingAfter = &next
			}
		}
	}
}

func retryPending(next *time.Time, now time.Time) bool {
	return next != nil && next.After(now)
}
