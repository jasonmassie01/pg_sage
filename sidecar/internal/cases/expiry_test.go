package cases

import (
	"testing"
	"time"
)

// No concurrent access tests: MarkExpiredCandidates mutates only the
// slice it is given, one request's projection, with no shared state.

var expiryNow = time.Date(2026, 11, 11, 14, 0, 0, 0, time.UTC)

func expiringCase(expires *time.Time, blocked string) Case {
	return Case{ID: "finding:prod:x", ActionCandidates: []ActionCandidate{{
		ActionType: "analyze", ExpiresAt: expires, BlockedReason: blocked,
	}}}
}

func at(offset time.Duration) *time.Time {
	t := expiryNow.Add(offset)
	return &t
}

// Boundary: 1 s past expires_at is expired, 1 s before is not, and the
// instant itself is expired (IsExecutable is strictly before).
func TestMarkExpiredCandidates_BoundaryAroundExpiresAt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expires *time.Time
		want    string
	}{
		{"one second past", at(-time.Second), ReasonEvidenceExpired},
		{"at the instant", at(0), ReasonEvidenceExpired},
		{"one second before", at(time.Second), ""},
		{"no expiry", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list := []Case{expiringCase(tc.expires, "")}
			MarkExpiredCandidates(list, expiryNow)
			if got := list[0].ActionCandidates[0].BlockedReason; got != tc.want {
				t.Fatalf("blocked_reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// An expired candidate's earlier restriction is replaced: stale evidence
// blocks it first. A recommendation refusal is kept, because a fresh
// observation of the same content would still be refused.
func TestMarkExpiredCandidates_ReasonPrecedence(t *testing.T) {
	list := []Case{
		expiringCase(at(-time.Hour), "planner hints require approval and revalidation"),
		expiringCase(at(-time.Hour), ReasonRecommendationReverted),
		expiringCase(at(-time.Hour), ReasonRecommendationAbandoned),
		expiringCase(at(time.Hour), "planner hints require approval and revalidation"),
	}
	MarkExpiredCandidates(list, expiryNow)
	want := []string{ReasonEvidenceExpired, ReasonRecommendationReverted,
		ReasonRecommendationAbandoned, "planner hints require approval and revalidation"}
	for i, w := range want {
		if got := list[i].ActionCandidates[0].BlockedReason; got != w {
			t.Fatalf("case %d blocked_reason = %q, want %q", i, got, w)
		}
	}
}

// Every candidate of every case is checked, not only the first.
func TestMarkExpiredCandidates_EveryCandidate(t *testing.T) {
	list := []Case{{ActionCandidates: []ActionCandidate{
		{ActionType: "a", ExpiresAt: at(time.Minute)},
		{ActionType: "b", ExpiresAt: at(-time.Minute)},
	}}, expiringCase(at(-time.Minute), "")}
	MarkExpiredCandidates(list, expiryNow)
	if got := list[0].ActionCandidates[0].BlockedReason; got != "" {
		t.Fatalf("fresh candidate blocked_reason = %q, want none", got)
	}
	if got := list[0].ActionCandidates[1].BlockedReason; got != ReasonEvidenceExpired {
		t.Fatalf("second candidate blocked_reason = %q, want expired", got)
	}
	if got := list[1].ActionCandidates[0].BlockedReason; got != ReasonEvidenceExpired {
		t.Fatalf("second case blocked_reason = %q, want expired", got)
	}
}

// Nil/empty: no cases, a case without candidates, and a zero clock do
// not panic; a zero clock expires nothing that has a real expiry.
func TestMarkExpiredCandidates_EmptyAndZero(t *testing.T) {
	MarkExpiredCandidates(nil, expiryNow)
	MarkExpiredCandidates([]Case{{ID: "x"}}, expiryNow)
	list := []Case{expiringCase(at(-time.Hour), "")}
	MarkExpiredCandidates(list, time.Time{})
	if got := list[0].ActionCandidates[0].BlockedReason; got != "" {
		t.Fatalf("zero clock blocked_reason = %q, want none", got)
	}
}

// A finding projected with an old observation carries an expiry the
// marker honours: last_seen + 24 h for a plain SQL candidate.
func TestMarkExpiredCandidates_ProjectedFindingExpiresAfterADay(t *testing.T) {
	seen := expiryNow.Add(-24 * time.Hour)
	c := ProjectFinding(SourceFinding{ID: "7", DatabaseName: "prod",
		Category: "stale_statistics", ObjectType: "table", ObjectIdentifier: "public.t",
		RecommendedSQL: "ANALYZE public.t", ObservedAt: seen}, testClassify)
	if len(c.ActionCandidates) != 1 || c.ActionCandidates[0].ExpiresAt == nil {
		t.Fatalf("projected candidates = %+v, want one with an expiry", c.ActionCandidates)
	}
	before := []Case{c}
	before[0].ActionCandidates = append([]ActionCandidate(nil), c.ActionCandidates...)
	MarkExpiredCandidates(before, expiryNow.Add(-time.Second))
	if got := before[0].ActionCandidates[0].BlockedReason; got == ReasonEvidenceExpired {
		t.Fatalf("a candidate 1 s before its expiry is marked expired")
	}
	MarkExpiredCandidates([]Case{c}, expiryNow.Add(time.Second))
	if got := c.ActionCandidates[0].BlockedReason; got != ReasonEvidenceExpired {
		t.Fatalf("a candidate 1 s past its expiry has blocked_reason %q", got)
	}
}
