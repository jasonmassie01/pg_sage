package chatops

import (
	"errors"
	"testing"
)

// Why a plan card could not be used, judged by the database clock that
// Consume's UPDATE uses (the sidecar's clock may differ from the server's):
// used beats expired, expired beats a decision the card did not offer.
// No concurrent access tests: TestPlanCardConsumedOnceUnderConcurrency
// covers two deciders racing for one card.

func TestPlanCardConsume_RefusalReasonsUseTheDatabaseClock(t *testing.T) {
	_, pool, ctx := liveStore(t)
	s := NewPlanCardStore(pool)
	expire := func(planID int64, by string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE sage.plan_card_deliveries
			SET expires_at = now() - $2::interval WHERE plan_id = $1`, planID, by); err != nil {
			t.Fatal(err)
		}
	}
	notOffered := planIssue(t)
	notOffered.Offered, notOffered.UpTo = []PlanChoice{PlanReject}, 0
	tok, err := s.Issue(ctx, notOffered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(ctx, tok, 1, PlanApproveNext, 0); !errors.Is(err,
		ErrChoiceNotOffered) {
		t.Fatalf("live card, choice not offered: %v, want ErrChoiceNotOffered", err)
	}
	expire(notOffered.PlanID, "1 millisecond") // just past, by the server's clock
	if _, err := s.Consume(ctx, tok, 1, PlanApproveNext, 0); !errors.Is(err, ErrCardExpired) {
		t.Fatalf("expired card, choice not offered: %v, want ErrCardExpired", err)
	}
	if _, err := s.Consume(ctx, tok, 1, PlanReject, 0); !errors.Is(err, ErrCardExpired) {
		t.Fatalf("expired card, offered choice: %v, want ErrCardExpired", err)
	}
	used := planIssue(t)
	utok, err := s.Issue(ctx, used)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(ctx, utok, 3, PlanReject, 0); err != nil {
		t.Fatalf("first use: %v", err)
	}
	expire(used.PlanID, "1 hour")
	if _, err := s.Consume(ctx, utok, 4, PlanReject, 0); !errors.Is(err, ErrCardUsed) {
		t.Fatalf("used then expired card: %v, want ErrCardUsed", err)
	}
	never := "AAAAAAAAAAAAAAAAAAAAAA" // a well-formed token no card was issued with
	if _, err := s.Consume(ctx, never, 1, PlanReject, 0); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("unknown token: %v, want ErrCardUnknown", err)
	}
}
