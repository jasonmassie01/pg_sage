package chatops

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// Fact cards (roadmap 2.3) reuse the approval-card token mechanism: a
// Confirm / Reject button carries only the decision and an opaque token
// naming one fact card sent to one channel.

func TestParseSlackFactDecisions(t *testing.T) {
	for actionID, want := range map[string]Decision{
		SlackFactConfirmAction: DecisionApprove, SlackFactRejectAction: DecisionDeny,
	} {
		got, err := ParseSlack(slackForm(t, slackPayload(actionID, cardToken)))
		if err != nil {
			t.Fatalf("%s: %v", actionID, err)
		}
		if got.Decision != want || got.FactToken != cardToken || got.CardToken != "" ||
			got.ProposalID != "" || got.UserID != "U0042" {
			t.Fatalf("%s parsed as %+v", actionID, got)
		}
	}
	if _, err := ParseSlack(slackForm(t, slackPayload(SlackFactConfirmAction,
		"short"))); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bad fact token: %v", err)
	}
}

func TestParseTelegramFactDecisions(t *testing.T) {
	for d, want := range map[Decision]Decision{DecisionApprove: DecisionApprove,
		DecisionDeny: DecisionDeny} {
		data := FactCallbackData(d, cardToken)
		if len(data) > 64 {
			t.Fatalf("callback data %d bytes", len(data))
		}
		got, err := ParseTelegram(telegramUpdate(data))
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		if got.Decision != want || got.FactToken != cardToken || got.CardToken != "" ||
			got.MessageID != 77 {
			t.Fatalf("%s parsed as %+v", d, got)
		}
	}
	if FactCallbackData(DecisionApprove, cardToken) == FactCallbackData(DecisionDeny,
		cardToken) || FactCallbackData(DecisionApprove, cardToken) == CardCallbackData(
		DecisionApprove, cardToken) {
		t.Fatal("fact callback data must be distinct per decision and from card data")
	}
	for _, data := range []string{"sage:fx:" + cardToken, "sage:fc:short", "sage:f",
		"sage:fc" + cardToken} {
		if _, err := ParseTelegram(telegramUpdate(data)); !errors.Is(err, ErrMalformed) {
			t.Fatalf("%q: %v", data, err)
		}
	}
	// An approval card still parses as one.
	got, err := ParseTelegram(telegramUpdate(CardCallbackData(DecisionApprove, cardToken)))
	if err != nil || got.CardToken != cardToken || got.FactToken != "" {
		t.Fatalf("approval card: %+v %v", got, err)
	}
}

func factIssue(factID int64) FactCardIssue {
	return FactCardIssue{ChannelID: 7, Database: "orders", FactID: factID,
		FactHash: "fh-" + time.Now().Format(time.RFC3339Nano),
		Title:    "Confirm: schemas test_* are test fixtures", ExpiresAt: time.Now().Add(time.Hour)}
}

func TestFactCardIssueLookupConsume(t *testing.T) {
	_, pool, ctx := liveStore(t)
	s := NewFactCardStore(pool)
	in := factIssue(12)
	token, err := s.Issue(ctx, in)
	if err != nil || !ValidCardToken(token) {
		t.Fatalf("issue %q %v", token, err)
	}
	got, err := s.Lookup(ctx, token)
	if err != nil || got.FactID != 12 || got.FactHash != in.FactHash || got.ChannelID != 7 ||
		got.Database != "orders" || got.UsedAt != nil {
		t.Fatalf("lookup %+v %v", got, err)
	}
	used, err := s.Consume(ctx, token, 3, DecisionApprove, 77)
	if err != nil || used.UsedAt == nil || used.Decision != "confirm" || *used.UsedBy != 3 {
		t.Fatalf("consume %+v %v", used, err)
	}
	if _, err := s.Consume(ctx, token, 3, DecisionDeny, 78); !errors.Is(err, ErrCardUsed) {
		t.Fatalf("second use: %v", err)
	}
	if _, err := s.Lookup(ctx, "AAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("unknown token: %v", err)
	}
	if _, err := s.Issue(ctx, FactCardIssue{ChannelID: 7, Database: "orders"}); !errors.Is(
		err, ErrMalformed) {
		t.Fatalf("invalid issue: %v", err)
	}
}

func TestFactCardExpiryRevokeAndConcurrency(t *testing.T) {
	_, pool, ctx := liveStore(t)
	s := NewFactCardStore(pool)
	token, err := s.Issue(ctx, factIssue(5))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.fact_card_deliveries SET expires_at = now() -
		interval '1 second' WHERE fact_id = 5`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(ctx, token, 1, DecisionApprove, 0); !errors.Is(err, ErrCardExpired) {
		t.Fatalf("expired: %v", err)
	}
	revoked, _ := s.Issue(ctx, factIssue(6))
	if err := s.Revoke(ctx, revoked); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lookup(ctx, revoked); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("revoked: %v", err)
	}
	once, _ := s.Issue(ctx, factIssue(8))
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(user int) {
			defer wg.Done()
			if _, err := s.Consume(ctx, once, user, DecisionApprove, 0); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i + 1)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("a fact card decided %d times", wins)
	}
}
