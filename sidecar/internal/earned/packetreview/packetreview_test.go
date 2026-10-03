package packetreview

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Phase 1.1: one operator review of a finished investigation writes both
// records that describe it: the shadow review behind earned autonomy
// (accepted / rejected) and the investigation outcome in incident memory
// (confirmed / refuted, with the actual root node when the operator names
// a graph node). Free-text root causes stay in the review note. Unit tests
// with fakes; the API tests run the same path against Postgres.

// No concurrent-access tests here: Record holds no state of its own; the
// ledger and investigation stores own concurrency (tested there).

const invID = "11111111-1111-4111-8111-111111111111"

type fakeLedger struct {
	db      string
	reviews []earned.Review
	err     error
}

func (l *fakeLedger) RecordReview(_ context.Context, r earned.Review) error {
	if l.err != nil {
		return l.err
	}
	l.reviews = append(l.reviews, r)
	return nil
}

func (l *fakeLedger) Database() string { return l.db }

type fakeInvestigations struct {
	inv       sre.Investigation
	detailErr error
	outErr    error
	outcomes  []sre.OutcomeRequest
}

func (f *fakeInvestigations) Detail(_ context.Context, id sre.UUID) (sre.Detail, error) {
	if f.detailErr != nil {
		return sre.Detail{}, f.detailErr
	}
	if string(id) != string(f.inv.ID) {
		return sre.Detail{}, sre.ErrNotFound
	}
	return sre.Detail{Database: "orders", Investigation: f.inv}, nil
}

func (f *fakeInvestigations) RecordOutcome(_ context.Context, _ sre.UUID,
	req sre.OutcomeRequest) (sre.Outcome, error) {
	if f.outErr != nil {
		return sre.Outcome{}, f.outErr
	}
	f.outcomes = append(f.outcomes, req)
	return sre.Outcome{Verdict: sre.OutcomeVerdict(req.Verdict), ActualNode: req.ActualNode,
		Actor: req.Actor}, nil
}

func concluded() *fakeInvestigations {
	return &fakeInvestigations{inv: sre.Investigation{ID: invID, State: sre.StateConcluded,
		TriggerKind: "lock_blocking", Summary: sre.Summary{Family: "lock_blocking",
			Root: "idle_in_tx_holder"}}}
}

func inconclusive() *fakeInvestigations {
	return &fakeInvestigations{inv: sre.Investigation{ID: invID, State: sre.StateInconclusive,
		TriggerKind: "lock_blocking", Summary: sre.Summary{}}}
}

func TestAcceptWritesTheReviewAndConfirmsTheInvestigation(t *testing.T) {
	ledger, inv := &fakeLedger{db: "orders"}, concluded()
	res, err := Record(context.Background(), ledger, inv, Request{InvestigationID: invID,
		Verdict: earned.VerdictAccepted, Note: "matches what we saw",
		Actor: "user:2:ops@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.reviews) != 1 || len(inv.outcomes) != 1 {
		t.Fatalf("writes = %d reviews, %d outcomes; want 1 and 1", len(ledger.reviews),
			len(inv.outcomes))
	}
	r := ledger.reviews[0]
	if r.Database != "orders" || r.InvestigationID != invID ||
		r.Family != earned.FamilyLockBlocking || r.Verdict != earned.VerdictAccepted ||
		r.Reviewer != "user:2:ops@example.com" || r.Note != "matches what we saw" {
		t.Fatalf("review = %+v", r)
	}
	o := inv.outcomes[0]
	if o.Verdict != string(sre.OutcomeConfirmed) || o.ActualNode != "" ||
		o.Actor != "user:2:ops@example.com" {
		t.Fatalf("outcome = %+v", o)
	}
	if res.Family != "lock_blocking" || res.Database != "orders" || res.Outcome == nil ||
		res.OutcomeSkipped != "" || res.Verdict != earned.VerdictAccepted {
		t.Fatalf("result = %+v", res)
	}
}

func TestRejectWithAGraphNodeRecordsTheActualRoot(t *testing.T) {
	ledger, inv := &fakeLedger{db: "orders"}, concluded()
	res, err := Record(context.Background(), ledger, inv, Request{InvestigationID: invID,
		Verdict: earned.VerdictRejected, ActualRootCause: " connection_leak ",
		Actor: "user:2:o@e"})
	if err != nil {
		t.Fatal(err)
	}
	if inv.outcomes[0].Verdict != string(sre.OutcomeRefuted) ||
		inv.outcomes[0].ActualNode != "connection_leak" || res.ActualNode != "connection_leak" {
		t.Fatalf("outcome = %+v, result = %+v", inv.outcomes, res)
	}
	if !strings.Contains(ledger.reviews[0].Note, "connection_leak") {
		t.Fatalf("review note = %q, want the actual root cause", ledger.reviews[0].Note)
	}
}

func TestFreeTextRootCauseStaysInTheNote(t *testing.T) {
	ledger, inv := &fakeLedger{db: "orders"}, concluded()
	res, err := Record(context.Background(), ledger, inv, Request{InvestigationID: invID,
		Verdict: earned.VerdictRejected, Note: "wrong", Actor: "user:2:o@e",
		ActualRootCause: "a cron job held an advisory lock"})
	if err != nil {
		t.Fatal(err)
	}
	note := ledger.reviews[0].Note
	if !strings.Contains(note, "wrong") || !strings.Contains(note,
		"actual root cause: a cron job held an advisory lock") {
		t.Fatalf("note = %q", note)
	}
	if inv.outcomes[0].ActualNode != "" || res.ActualNode != "" {
		t.Fatalf("free text became a graph node: %+v", inv.outcomes[0])
	}
}

// Accepting an inconclusive investigation is a valid review (the packet
// was right not to conclude), but incident memory confirms only a
// conclusion or a named graph node; the outcome is skipped and said so.
func TestAcceptInconclusiveWithoutANodeSkipsTheOutcome(t *testing.T) {
	ledger, inv := &fakeLedger{db: "orders"}, inconclusive()
	res, err := Record(context.Background(), ledger, inv, Request{InvestigationID: invID,
		Verdict: earned.VerdictAccepted, Actor: "user:2:o@e"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.reviews) != 1 || len(inv.outcomes) != 0 || res.Outcome != nil ||
		res.OutcomeSkipped == "" {
		t.Fatalf("result = %+v, outcomes %+v", res, inv.outcomes)
	}
	// The trigger kind is the family when the summary has none.
	if ledger.reviews[0].Family != earned.FamilyLockBlocking {
		t.Fatalf("family = %q", ledger.reviews[0].Family)
	}
	// RequireOutcome (the investigation outcome route) refuses before any write.
	ledger2, inv2 := &fakeLedger{db: "orders"}, inconclusive()
	_, err = Record(context.Background(), ledger2, inv2, Request{InvestigationID: invID,
		Verdict: earned.VerdictAccepted, Actor: "user:2:o@e", RequireOutcome: true})
	if !errors.Is(err, earned.ErrInvalidRequest) || len(ledger2.reviews) != 0 {
		t.Fatalf("strict = %v, reviews %d", err, len(ledger2.reviews))
	}
	ledger3, inv3 := &fakeLedger{db: "orders"}, concluded()
	_, err = Record(context.Background(), ledger3, inv3, Request{InvestigationID: invID,
		Verdict: earned.VerdictRejected, Actor: "user:2:o@e", RequireOutcome: true,
		ActualRootCause: "not a node"})
	if !errors.Is(err, earned.ErrInvalidRequest) || len(ledger3.reviews) != 0 {
		t.Fatalf("strict free text = %v, reviews %d", err, len(ledger3.reviews))
	}
}

func TestOnlyFinishedInvestigationsAreReviewed(t *testing.T) {
	for _, state := range []sre.State{sre.StateCollecting, sre.StateFailed, "paused"} {
		inv := concluded()
		inv.inv.State = state
		ledger := &fakeLedger{db: "orders"}
		_, err := Record(context.Background(), ledger, inv, Request{InvestigationID: invID,
			Verdict: earned.VerdictAccepted, Actor: "user:2:o@e"})
		if !errors.Is(err, ErrNotFinished) || len(ledger.reviews)+len(inv.outcomes) != 0 {
			t.Errorf("%s: %v (%d writes)", state, err, len(ledger.reviews)+len(inv.outcomes))
		}
	}
}

func TestInvalidRequestsWriteNothing(t *testing.T) {
	for name, req := range map[string]Request{
		"verdict":   {InvestigationID: invID, Verdict: "confirmed", Actor: "user:2:o@e"},
		"empty":     {InvestigationID: invID, Actor: "user:2:o@e"},
		"id":        {InvestigationID: "x' OR 1=1", Verdict: "accepted", Actor: "user:2:o@e"},
		"no id":     {Verdict: "accepted", Actor: "user:2:o@e"},
		"no actor":  {InvestigationID: invID, Verdict: "accepted"},
		"long note": {InvestigationID: invID, Verdict: "accepted", Actor: "u",
			Note: strings.Repeat("n", 1501)},
		"long root": {InvestigationID: invID, Verdict: "accepted", Actor: "u",
			ActualRootCause: strings.Repeat("r", 401)},
	} {
		ledger, inv := &fakeLedger{db: "orders"}, concluded()
		_, err := Record(context.Background(), ledger, inv, req)
		if !errors.Is(err, earned.ErrInvalidRequest) ||
			len(ledger.reviews)+len(inv.outcomes) != 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Record(context.Background(), &fakeLedger{db: "orders"}, nil,
		Request{InvestigationID: invID, Verdict: "accepted", Actor: "u"}); !errors.Is(err,
		ErrUnavailable) {
		t.Fatalf("no investigations: %v", err)
	}
	if _, err := Record(context.Background(), nil, concluded(),
		Request{InvestigationID: invID, Verdict: "accepted", Actor: "u"}); !errors.Is(err,
		ErrUnavailable) {
		t.Fatalf("no ledger: %v", err)
	}
}

func TestErrorsPropagateDistinguishably(t *testing.T) {
	inv := concluded()
	inv.detailErr = sre.ErrNotFound
	ledger := &fakeLedger{db: "orders"}
	if _, err := Record(context.Background(), ledger, inv, Request{InvestigationID: invID,
		Verdict: "accepted", Actor: "u"}); !errors.Is(err, sre.ErrNotFound) {
		t.Fatalf("unknown investigation: %v", err)
	}
	ledger = &fakeLedger{db: "orders", err: earned.ErrUnavailable}
	inv = concluded()
	if _, err := Record(context.Background(), ledger, inv, Request{InvestigationID: invID,
		Verdict: "accepted", Actor: "u"}); !errors.Is(err, earned.ErrUnavailable) ||
		len(inv.outcomes) != 0 {
		t.Fatalf("ledger down: %v, outcomes %d (must not write the outcome alone)", err,
			len(inv.outcomes))
	}
	ledger = &fakeLedger{db: "orders"}
	inv = concluded()
	inv.outErr = errors.New("memory store down")
	_, err := Record(context.Background(), ledger, inv, Request{InvestigationID: invID,
		Verdict: "accepted", Actor: "u"})
	if err == nil || !strings.Contains(err.Error(), "memory store down") ||
		!strings.Contains(err.Error(), "review recorded") || len(ledger.reviews) != 1 {
		t.Fatalf("outcome failure after the review = %v (reviews %d)", err,
			len(ledger.reviews))
	}
}

// The investigation store caps actors at 128 characters; a longer
// reviewer identity is shortened for the outcome, kept whole for the
// review.
func TestLongActorIsShortenedForTheOutcomeOnly(t *testing.T) {
	ledger, inv := &fakeLedger{db: "orders"}, concluded()
	actor := "user:2:" + strings.Repeat("a", 150) + "@example.com"
	if _, err := Record(context.Background(), ledger, inv, Request{InvestigationID: invID,
		Verdict: "accepted", Actor: actor}); err != nil {
		t.Fatal(err)
	}
	if ledger.reviews[0].Reviewer != actor || len(inv.outcomes[0].Actor) != 128 {
		t.Fatalf("reviewer %d chars, outcome actor %d chars", len(ledger.reviews[0].Reviewer),
			len(inv.outcomes[0].Actor))
	}
}

// Coordinator decision 2026-10-02: a review through MCP is recorded but
// never counts toward promotion, and the result says so; a person's
// review counts.
func TestResultSaysWhetherTheReviewCounts(t *testing.T) {
	for actor, counts := range map[string]bool{"user:2:o@e": true, "user:2": true,
		"mcp:user:2": false, "mcp:stdio": false} {
		ledger, inv := &fakeLedger{db: "orders"}, concluded()
		res, err := Record(context.Background(), ledger, inv, Request{InvestigationID: invID,
			Verdict: earned.VerdictAccepted, Actor: actor})
		if err != nil {
			t.Fatal(err)
		}
		if res.CountsTowardPromotion != counts || len(ledger.reviews) != 1 {
			t.Errorf("%s: counts = %v, want %v", actor, res.CountsTowardPromotion, counts)
		}
		if !counts && (!strings.Contains(res.Notice, "does not count") ||
			!strings.Contains(res.Notice, "person")) {
			t.Errorf("%s: notice = %q", actor, res.Notice)
		}
		if counts && res.Notice != "" {
			t.Errorf("%s: a counting review carries a notice %q", actor, res.Notice)
		}
	}
}
