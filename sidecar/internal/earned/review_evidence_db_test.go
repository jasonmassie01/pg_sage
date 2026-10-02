package earned

import (
	"errors"
	"testing"
	"time"
)

// Coordinator decision 2026-10-02: an agent must not be able to generate
// its own trust evidence. A review recorded through MCP (actor "mcp:...")
// is kept, but only a review by a human session (UI or REST, actor
// "user:<id>...") counts as shadow evidence for promotion. An agent's
// review never replaces a person's review of the same investigation.

func TestOnlyHumanSessionReviewsCountAsEvidence(t *testing.T) {
	for actor, want := range map[string]bool{
		"user:7": true, "user:7:ops@example.com": true, "user:12:a:b": true,
		"mcp:user:7": false, "mcp:stdio": false, "user:": false, "user:x": false,
		"User:7": false, " user:7": false, "": false, "system": false,
	} {
		if got := ReviewCountsAsEvidence(actor); got != want {
			t.Errorf("ReviewCountsAsEvidence(%q) = %v, want %v", actor, got, want)
		}
	}
}

func (f *fixture) review(id, reviewer, verdict string) error {
	f.t.Helper()
	return f.svc.RecordReview(f.ctx, Review{Database: f.db, InvestigationID: id,
		Family: FamilyLockBlocking, Verdict: verdict, Reviewer: reviewer})
}

func (f *fixture) shadowSince(since time.Time) Shadow {
	f.t.Helper()
	sh, err := f.store.ShadowStats(f.ctx, FamilyLockBlocking, since)
	if err != nil {
		f.t.Fatal(err)
	}
	return sh
}

func TestAgentReviewIsRecordedButEarnsNothing(t *testing.T) {
	f := newFixture(t)
	id := newUUID(t)
	if err := f.review(id, "mcp:user:2", VerdictAccepted); err != nil {
		t.Fatalf("an MCP review must be recorded: %v", err)
	}
	var n int
	var counts bool
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*), bool_or(counts_as_evidence)
		FROM sage.sre_packet_reviews WHERE deployment_id = $1 AND investigation_id = $2`,
		f.store.DeploymentID(), id).Scan(&n, &counts); err != nil || n != 1 || counts {
		t.Fatalf("stored review = %d rows, counts %v (%v)", n, counts, err)
	}
	if sh := f.shadowSince(time.Time{}); sh.Reviewed != 0 || sh.Accepted != 0 ||
		!sh.FirstReviewAt.IsZero() {
		t.Fatalf("shadow after an agent review = %+v, want nothing", sh)
	}
}

func TestAgentReviewNeverReplacesAPersonsReview(t *testing.T) {
	f := newFixture(t)
	id := newUUID(t)
	if err := f.review(id, "user:7:o@e", VerdictRejected); err != nil {
		t.Fatal(err)
	}
	err := f.review(id, "mcp:user:7", VerdictAccepted)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("agent review over a person's = %v, want a conflict", err)
	}
	if sh := f.shadowSince(time.Time{}); sh.Reviewed != 1 || sh.Accepted != 0 {
		t.Fatalf("shadow = %+v: the person's rejection must stand", sh)
	}
	// A person may replace an agent's review, and then it counts.
	other := newUUID(t)
	if err := f.review(other, "mcp:user:7", VerdictRejected); err != nil {
		t.Fatal(err)
	}
	if err := f.review(other, "user:8:p@e", VerdictAccepted); err != nil {
		t.Fatalf("person over agent: %v", err)
	}
	if sh := f.shadowSince(time.Time{}); sh.Reviewed != 2 || sh.Accepted != 1 {
		t.Fatalf("shadow = %+v, want the two persons' reviews", sh)
	}
	// An agent may revise its own review.
	third := newUUID(t)
	for _, v := range []string{VerdictAccepted, VerdictRejected} {
		if err := f.review(third, "mcp:user:7", v); err != nil {
			t.Fatalf("agent revising its own review: %v", err)
		}
	}
}

// Agent reviews alone never make a pair proposable.
func TestAgentReviewsNeverPromote(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	f.clock.Set(now.Add(-31 * 24 * time.Hour))
	for i := 0; i < 25; i++ {
		if i == 1 {
			f.clock.Set(now.Add(-time.Hour))
		}
		if err := f.review(newUUID(t), "mcp:user:2", VerdictAccepted); err != nil {
			t.Fatal(err)
		}
	}
	f.clock.Set(now)
	if _, err := f.svc.IngestEvalRun(f.ctx, benchReport(now.Add(-time.Hour),
		FamilyLockBlocking), SourceBench, "user:1:admin@example.com", ""); err != nil {
		t.Fatal(err)
	}
	e, err := f.svc.Evaluate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range e.Created {
		if p.Family == FamilyLockBlocking {
			t.Fatalf("agent reviews proposed %+v", p)
		}
	}
	np := notProposed(e, FamilyLockBlocking, ClassBackendCancel)
	if np == nil || np.Reason != NotProposedEvidence {
		t.Fatalf("lock_blocking/backend_cancel = %+v", np)
	}
}
