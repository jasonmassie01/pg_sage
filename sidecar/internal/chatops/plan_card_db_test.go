package chatops

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// plan-03b: plan cards in the control database. A token names one card
// sent to one channel; it is used once, refused when expired, used or for
// a decision the card did not offer, and a consumed card can be released
// by its decider when the plan store could not record the decision.

var planCardSeq atomic.Int64

func planIssue(t *testing.T) PlanCardIssue {
	t.Helper()
	id := time.Now().UnixNano()%1e12 + planCardSeq.Add(1)
	return PlanCardIssue{ChannelID: 7, Database: "orders", PlanID: id,
		PlanHash: strings.Repeat("ab", 32), Title: "cut blocks per call",
		Offered: []PlanChoice{PlanApproveUpTo, PlanApproveNext, PlanReject}, UpTo: 2,
		ExpiresAt: time.Now().Add(time.Hour)}
}

func TestPlanCardIssueLookupConsume(t *testing.T) {
	_, pool, ctx := liveStore(t)
	s := NewPlanCardStore(pool)
	in := planIssue(t)
	token, err := s.Issue(ctx, in)
	if err != nil || !ValidCardToken(token) {
		t.Fatalf("issue %q %v", token, err)
	}
	got, err := s.Lookup(ctx, token)
	if err != nil || got.PlanID != in.PlanID || got.PlanHash != in.PlanHash ||
		got.ChannelID != 7 || got.Database != "orders" || got.UsedAt != nil ||
		got.UpTo != 2 || len(got.Offered) != 3 || !got.Offers(PlanApproveUpTo) ||
		got.Title != in.Title || got.Decision != "" {
		t.Fatalf("lookup %+v %v", got, err)
	}
	if _, err := s.Consume(ctx, token, 3, PlanApproveAll, 77); !errors.Is(err,
		ErrChoiceNotOffered) {
		t.Fatalf("a decision the card did not offer: %v", err)
	}
	used, err := s.Consume(ctx, token, 3, PlanApproveUpTo, 77)
	if err != nil || used.UsedAt == nil || used.Decision != PlanApproveUpTo ||
		used.UsedBy == nil || *used.UsedBy != 3 || used.MessageID != 77 {
		t.Fatalf("consume %+v %v", used, err)
	}
	if _, err := s.Consume(ctx, token, 4, PlanReject, 78); !errors.Is(err, ErrCardUsed) {
		t.Fatalf("second use: %v", err)
	}
	if _, err := s.Lookup(ctx, "AAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("unknown token: %v", err)
	}
	if _, err := s.Consume(ctx, "bad", 3, PlanReject, 0); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("malformed token: %v", err)
	}
}

func TestPlanCardIssueRefusesMalformedCards(t *testing.T) {
	_, pool, ctx := liveStore(t)
	s := NewPlanCardStore(pool)
	cases := map[string]func(*PlanCardIssue){
		"no channel":  func(in *PlanCardIssue) { in.ChannelID = 0 },
		"no plan":     func(in *PlanCardIssue) { in.PlanID = 0 },
		"no database": func(in *PlanCardIssue) { in.Database = " " },
		"bad hash":    func(in *PlanCardIssue) { in.PlanHash = "abc" },
		"expired":     func(in *PlanCardIssue) { in.ExpiresAt = time.Now() },
		"no reject": func(in *PlanCardIssue) {
			in.Offered, in.UpTo = []PlanChoice{PlanApproveNext}, 0
		},
		"unknown choice": func(in *PlanCardIssue) {
			in.Offered, in.UpTo = []PlanChoice{"x", PlanReject}, 0
		},
		"up_to no ordinal":   func(in *PlanCardIssue) { in.UpTo = 0 },
		"ordinal no button":  func(in *PlanCardIssue) { in.Offered = []PlanChoice{PlanReject} },
		"ordinal past steps": func(in *PlanCardIssue) { in.UpTo = 51 },
		"nil offer":          func(in *PlanCardIssue) { in.Offered, in.UpTo = nil, 0 },
	}
	for name, mutate := range cases {
		in := planIssue(t)
		mutate(&in)
		if _, err := s.Issue(ctx, in); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want ErrMalformed", name, err)
		}
	}
	rejectOnly := planIssue(t)
	rejectOnly.Offered, rejectOnly.UpTo = []PlanChoice{PlanReject}, 0
	if _, err := s.Issue(ctx, rejectOnly); err != nil {
		t.Fatalf("a reject-only card was refused: %v", err)
	}
}

func TestPlanCardExpiryReleaseRevoke(t *testing.T) {
	_, pool, ctx := liveStore(t)
	s := NewPlanCardStore(pool)
	in := planIssue(t)
	token, err := s.Issue(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.plan_card_deliveries SET expires_at = now() -
		interval '1 second' WHERE plan_id = $1`, in.PlanID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(ctx, token, 1, PlanReject, 0); !errors.Is(err, ErrCardExpired) {
		t.Fatalf("expired: %v", err)
	}
	// Release: only the decider frees a used card; then it decides again.
	held, _ := s.Issue(ctx, planIssue(t))
	if _, err := s.Consume(ctx, held, 5, PlanApproveNext, 9); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, held, 6); !errors.Is(err, ErrCardUsed) {
		t.Fatalf("another user released the card: %v", err)
	}
	if err := s.Release(ctx, held, 5); err != nil {
		t.Fatalf("release: %v", err)
	}
	back, err := s.Lookup(ctx, held)
	if err != nil || back.UsedAt != nil || back.UsedBy != nil || back.Decision != "" {
		t.Fatalf("released card %+v %v", back, err)
	}
	if _, err := s.Consume(ctx, held, 6, PlanReject, 10); err != nil {
		t.Fatalf("a released card cannot decide: %v", err)
	}
	if err := s.Release(ctx, "bad", 6); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("release of a malformed token: %v", err)
	}
	revoked, _ := s.Issue(ctx, planIssue(t))
	if err := s.Revoke(ctx, revoked); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, revoked); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("revoked: %v", err)
	}
}

func TestPlanCardConsumedOnceUnderConcurrency(t *testing.T) {
	_, pool, ctx := liveStore(t)
	s := NewPlanCardStore(pool)
	once, err := s.Issue(ctx, planIssue(t))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var wins, used atomic.Int32
	for i := range 8 {
		wg.Add(1)
		go func(user int) {
			defer wg.Done()
			_, err := s.Consume(ctx, once, user, PlanReject, 0)
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrCardUsed):
				used.Add(1)
			}
		}(i + 1)
	}
	wg.Wait()
	if wins.Load() != 1 || used.Load() != 7 {
		t.Fatalf("a plan card decided %d times (%d refused as used)", wins.Load(),
			used.Load())
	}
}
