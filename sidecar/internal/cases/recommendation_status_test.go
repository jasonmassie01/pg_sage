package cases

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/recommendation"
)

// No concurrent access tests: AttachRecommendation mutates only the case
// it is given, and the head is a value read once per request.

const statusURL = "/api/v1/recommendations/9?database=prod"

func statusFinding() SourceFinding {
	return SourceFinding{ID: "7", DatabaseName: "prod", Category: "stale_statistics",
		ObjectType: "table", ObjectIdentifier: "public.orders",
		RecommendedSQL: "ANALYZE public.orders", ObservedAt: expiryNow}
}

// headFor is a head in state whose content hash is that of f's
// recommended SQL, as the analyzer proposed it in database prod.
func headFor(f SourceFinding, state recommendation.State) recommendation.FindingHead {
	hash := recommendation.ContentHash(recommendation.Proposal{DatabaseName: "prod",
		Category: f.Category, Target: f.ObjectIdentifier, ForwardSQL: f.RecommendedSQL,
		InverseSQL: f.RollbackSQL})
	applied := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	findingID := int64(7)
	return recommendation.FindingHead{Recommendation: recommendation.Recommendation{
		ID: 9, DatabaseName: "prod", FindingID: &findingID, State: state, Revision: 2,
		ContentHash: hash, AttemptCount: 1, UpdatedAt: applied.Add(time.Hour)},
		AppliedAt: &applied}
}

func attached(t *testing.T, head recommendation.FindingHead) Case {
	t.Helper()
	c := projectFinding(statusFinding())
	if len(c.ActionCandidates) != 1 {
		t.Fatalf("projected candidates = %d, want 1", len(c.ActionCandidates))
	}
	AttachRecommendation(&c, head, statusURL, expiryNow)
	if c.Recommendation == nil {
		t.Fatal("AttachRecommendation left no recommendation on the case")
	}
	return c
}

// State transitions: each head state sets the field and its own next step.
func TestAttachRecommendation_EveryStateHasItsOwnNextStep(t *testing.T) {
	seen := map[string]recommendation.State{}
	for _, state := range recommendation.AllStates() {
		head := headFor(statusFinding(), state)
		c := attached(t, head)
		r := c.Recommendation
		if r.ID != 9 || r.State != string(state) || r.Revision != 2 || r.URL != statusURL ||
			!r.CurrentContent || !r.UpdatedAt.Equal(head.UpdatedAt) {
			t.Fatalf("%s: recommendation = %+v", state, r)
		}
		if r.NextStep == "" {
			t.Fatalf("%s: empty next step", state)
		}
		if prev, dup := seen[r.NextStep]; dup {
			t.Fatalf("%s and %s share next step %q", state, prev, r.NextStep)
		}
		seen[r.NextStep] = state
	}
}

func TestAttachRecommendation_RevertedBlocksTheSameContent(t *testing.T) {
	head := headFor(statusFinding(), recommendation.StateReverted)
	head.Reason = "p95 latency rose 40%"
	c := attached(t, head)
	if got := c.ActionCandidates[0].BlockedReason; got != ReasonRecommendationReverted {
		t.Fatalf("blocked_reason = %q, want %q", got, ReasonRecommendationReverted)
	}
	want := "pg_sage applied this on 2026-11-10 and reverted it: p95 latency rose 40%"
	if c.Recommendation.NextStep != want {
		t.Fatalf("next step = %q, want %q", c.Recommendation.NextStep, want)
	}
	if c.Recommendation.Reason != head.Reason || c.Recommendation.AppliedAt == nil {
		t.Fatalf("recommendation = %+v, want the reason and applied_at", c.Recommendation)
	}
}

func TestAttachRecommendation_AbandonedBlocksTheSameContent(t *testing.T) {
	head := headFor(statusFinding(), recommendation.StateAbandoned)
	head.AttemptCount = 3
	c := attached(t, head)
	if got := c.ActionCandidates[0].BlockedReason; got != ReasonRecommendationAbandoned {
		t.Fatalf("blocked_reason = %q, want %q", got, ReasonRecommendationAbandoned)
	}
	if got := c.Recommendation.NextStep; got != "Stopped after 3 failed attempts" {
		t.Fatalf("next step = %q", got)
	}
	head.AttemptCount = 1
	if got := attached(t, head).Recommendation.NextStep; got != "Stopped after 1 failed attempt" {
		t.Fatalf("one-attempt next step = %q", got)
	}
}

// A failed head in backoff sets retrying_after; one that is due does not.
func TestAttachRecommendation_FailedInBackoffSetsRetryingAfter(t *testing.T) {
	head := headFor(statusFinding(), recommendation.StateFailed)
	next := expiryNow.Add(5 * time.Minute)
	head.NextAttemptAt = &next
	c := attached(t, head)
	got := c.ActionCandidates[0].RetryingAfter
	if got == nil || !got.Equal(next) {
		t.Fatalf("retrying_after = %v, want %v", got, next)
	}
	if c.ActionCandidates[0].BlockedReason != "" {
		t.Fatalf("a retry in backoff blocked the candidate: %q",
			c.ActionCandidates[0].BlockedReason)
	}
	if step := c.Recommendation.NextStep; step != "Retrying after 14:05 UTC" {
		t.Fatalf("next step = %q", step)
	}
	tomorrow := expiryNow.Add(26 * time.Hour)
	head.NextAttemptAt = &tomorrow
	if step := attached(t, head).Recommendation.NextStep; step !=
		"Retrying after 2026-11-12 16:00 UTC" {
		t.Fatalf("next-day next step = %q", step)
	}
	due := expiryNow.Add(-time.Second)
	head.NextAttemptAt = &due
	c = attached(t, head)
	if c.ActionCandidates[0].RetryingAfter != nil {
		t.Fatalf("a due retry set retrying_after %v", c.ActionCandidates[0].RetryingAfter)
	}
	if !strings.Contains(c.Recommendation.NextStep, "retry is due") {
		t.Fatalf("due next step = %q", c.Recommendation.NextStep)
	}
}

// Nil/empty: a reverted head of different content does not block the
// new candidate, and says the current proposal differs.
func TestAttachRecommendation_DifferentContentDoesNotBlock(t *testing.T) {
	for _, state := range []recommendation.State{recommendation.StateReverted,
		recommendation.StateAbandoned, recommendation.StateFailed} {
		head := headFor(statusFinding(), state)
		head.ContentHash = "an-earlier-hash"
		next := expiryNow.Add(time.Hour)
		head.NextAttemptAt = &next
		c := attached(t, head)
		cand := c.ActionCandidates[0]
		if cand.BlockedReason != "" || cand.RetryingAfter != nil ||
			c.Recommendation.CurrentContent {
			t.Fatalf("%s of other content touched the candidate: %+v / %+v",
				state, cand.BlockedReason, c.Recommendation)
		}
		if !strings.Contains(c.Recommendation.NextStep, "current proposal differs") {
			t.Fatalf("%s other-content next step = %q", state, c.Recommendation.NextStep)
		}
	}
}

// A head of other content in any other known state never claims the
// current proposal: its next step says the proposal differs (an applied
// head is held while the analyzer already proposes new SQL).
func TestAttachRecommendation_OtherContentEveryStateSaysItDiffers(t *testing.T) {
	for _, state := range recommendation.AllStates() {
		head := headFor(statusFinding(), state)
		head.ContentHash = "an-earlier-hash"
		c := attached(t, head)
		if c.Recommendation.CurrentContent || c.ActionCandidates[0].BlockedReason != "" {
			t.Fatalf("%s of other content: %+v / %q", state, c.Recommendation,
				c.ActionCandidates[0].BlockedReason)
		}
		if !strings.HasSuffix(c.Recommendation.NextStep, "; the current proposal differs") {
			t.Fatalf("%s other-content next step = %q", state, c.Recommendation.NextStep)
		}
	}
}

// A refused head blocks only the candidate that runs its SQL, never a
// second candidate with other SQL.
func TestAttachRecommendation_BlocksOnlyTheMatchingCandidate(t *testing.T) {
	c := projectFinding(statusFinding())
	other := c.ActionCandidates[0]
	other.ActionType, other.ProposedSQL = "cancel_backend", "SELECT pg_cancel_backend(1)"
	c.ActionCandidates = append(c.ActionCandidates, other)
	AttachRecommendation(&c, headFor(statusFinding(), recommendation.StateReverted),
		statusURL, expiryNow)
	if c.ActionCandidates[0].BlockedReason != ReasonRecommendationReverted ||
		c.ActionCandidates[1].BlockedReason != "" {
		t.Fatalf("blocked reasons = %q / %q", c.ActionCandidates[0].BlockedReason,
			c.ActionCandidates[1].BlockedReason)
	}
}

// Invalid input: a state from a future version is reported as unknown
// and blocks nothing.
func TestAttachRecommendation_UnknownStateIsNotAttached(t *testing.T) {
	head := headFor(statusFinding(), recommendation.State("rolled_forward"))
	c := attached(t, head)
	if c.ActionCandidates[0].BlockedReason != "" {
		t.Fatalf("unknown state blocked the candidate")
	}
	if step := c.Recommendation.NextStep; !strings.Contains(step, "state unknown") ||
		!strings.Contains(step, "rolled_forward") {
		t.Fatalf("unknown-state next step = %q", step)
	}
}

// Nil/empty: a case without candidates still carries the head, and a
// reverted head without an apply time or reason says so plainly.
func TestAttachRecommendation_NoCandidatesNoAppliedAt(t *testing.T) {
	c := Case{ID: "finding:prod:x", DatabaseName: "prod"}
	head := headFor(statusFinding(), recommendation.StateReverted)
	head.AppliedAt = nil
	AttachRecommendation(&c, head, statusURL, expiryNow)
	if c.Recommendation == nil || c.Recommendation.CurrentContent {
		t.Fatalf("recommendation = %+v, want attached without current content", c.Recommendation)
	}
	AttachRecommendation(nil, head, statusURL, expiryNow)
	c = attached(t, head)
	want := "pg_sage applied this and reverted it: no reason was recorded"
	if c.Recommendation.NextStep != want {
		t.Fatalf("next step = %q, want %q", c.Recommendation.NextStep, want)
	}
}

func TestMarkRecommendationUnknown(t *testing.T) {
	c := projectFinding(statusFinding())
	MarkRecommendationUnknown(&c)
	MarkRecommendationUnknown(nil)
	if !c.RecommendationUnknown || c.Recommendation != nil {
		t.Fatalf("case = unknown %v / %+v", c.RecommendationUnknown, c.Recommendation)
	}
}
