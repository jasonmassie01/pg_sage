package chatops

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A card token is minted per card per channel, stored only as its
// SHA-256, valid until it expires, and consumed at most once.

func cardIssue(queueID int) CardIssue {
	return CardIssue{ChannelID: 7, Database: "orders", QueueID: queueID,
		CardHash: "h-" + time.Now().Format(time.RFC3339Nano), Title: "Create index",
		Summary: "38% faster on 3 queries", ExpiresAt: time.Now().Add(time.Hour)}
}

func liveCards(t *testing.T) (*CardStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	_, pool, ctx := liveStore(t)
	return NewCardStore(pool), pool, ctx
}

func TestCardIssueLookupStoresOnlyTheHash(t *testing.T) {
	s, pool, ctx := liveCards(t)
	in := cardIssue(101)
	token, err := s.Issue(ctx, in)
	if err != nil || !ValidCardToken(token) {
		t.Fatalf("issue = %q, %v", token, err)
	}
	other, err := s.Issue(ctx, in)
	if err != nil || other == token {
		t.Fatalf("second token %q (first %q), %v", other, token, err)
	}
	var raw int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.approval_card_deliveries d
		WHERE row_to_json(d)::text LIKE '%' || $1 || '%'`, token).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	var hashed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.approval_card_deliveries
		WHERE token_sha256 = $1`, sum[:]).Scan(&hashed); err != nil {
		t.Fatal(err)
	}
	if raw != 0 || hashed != 1 {
		t.Fatalf("raw matches %d, hashed matches %d", raw, hashed)
	}
	got, err := s.Lookup(ctx, token)
	if err != nil || got.ChannelID != 7 || got.Database != "orders" || got.QueueID != 101 ||
		got.CardHash != in.CardHash || got.Title != "Create index" ||
		got.Summary != in.Summary || got.UsedAt != nil || got.ID <= 0 ||
		!got.ExpiresAt.After(time.Now()) {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
}

func TestCardIssueValidates(t *testing.T) {
	s, _, ctx := liveCards(t)
	for name, mutate := range map[string]func(*CardIssue){
		"no channel":   func(c *CardIssue) { c.ChannelID = 0 },
		"no database":  func(c *CardIssue) { c.Database = "" },
		"no queue":     func(c *CardIssue) { c.QueueID = 0 },
		"no hash":      func(c *CardIssue) { c.CardHash = "" },
		"already over": func(c *CardIssue) { c.ExpiresAt = time.Now().Add(-time.Second) },
	} {
		in := cardIssue(1)
		mutate(&in)
		if _, err := s.Issue(ctx, in); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
}

func TestCardLookupUnknownAndMalformed(t *testing.T) {
	s, _, ctx := liveCards(t)
	if _, err := s.Lookup(ctx, "ZZZZZZZZZZZZZZZZZZZZZZ"); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("forged token: err = %v, want ErrCardUnknown", err)
	}
	if _, err := s.Lookup(ctx, "short"); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("malformed token: err = %v, want ErrCardUnknown", err)
	}
	if _, err := s.Consume(ctx, "ZZZZZZZZZZZZZZZZZZZZZZ", 1, DecisionApprove,
		0); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("consume forged: err = %v", err)
	}
}

func TestCardConsumeOnceThenReplay(t *testing.T) {
	s, _, ctx := liveCards(t)
	token, err := s.Issue(ctx, cardIssue(202))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Consume(ctx, token, 5, DecisionApprove, 9001)
	if err != nil || got.UsedAt == nil || got.UsedBy == nil || *got.UsedBy != 5 ||
		got.Decision != string(DecisionApprove) || got.MessageID != 9001 || got.QueueID != 202 {
		t.Fatalf("consume = %+v, %v", got, err)
	}
	if _, err := s.Consume(ctx, token, 6, DecisionDeny, 0); !errors.Is(err, ErrCardUsed) {
		t.Fatalf("replay: err = %v, want ErrCardUsed", err)
	}
	again, err := s.Lookup(ctx, token)
	if err != nil || again.Decision != string(DecisionApprove) || *again.UsedBy != 5 {
		t.Fatalf("a replay changed the record: %+v, %v", again, err)
	}
}

func TestCardConsumeExpired(t *testing.T) {
	s, pool, ctx := liveCards(t)
	token, err := s.Issue(ctx, cardIssue(303))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	if _, err := pool.Exec(ctx, `UPDATE sage.approval_card_deliveries
		SET expires_at = now() - interval '1 second' WHERE token_sha256 = $1`,
		sum[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(ctx, token, 5, DecisionApprove, 0); !errors.Is(err, ErrCardExpired) {
		t.Fatalf("expired: err = %v, want ErrCardExpired", err)
	}
	got, err := s.Lookup(ctx, token)
	if err != nil || got.UsedAt != nil || !got.Expired(time.Now()) {
		t.Fatalf("expired card = %+v, %v", got, err)
	}
}

func TestCardRevoke(t *testing.T) {
	s, _, ctx := liveCards(t)
	token, err := s.Issue(ctx, cardIssue(404))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.Lookup(ctx, token); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("revoked card: err = %v", err)
	}
	if err := s.Revoke(ctx, token); err != nil {
		t.Fatalf("revoking twice must be harmless: %v", err)
	}
}

// Concurrent presses of one card (two approvers, a double click):
// exactly one consumes it.
func TestCardConsumeConcurrentExactlyOnce(t *testing.T) {
	s, _, ctx := liveCards(t)
	token, err := s.Issue(ctx, cardIssue(505))
	if err != nil {
		t.Fatal(err)
	}
	const n = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, used := 0, 0
	for i := range n {
		wg.Add(1)
		go func(user int) {
			defer wg.Done()
			_, err := s.Consume(ctx, token, user, DecisionApprove, 0)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrCardUsed):
				used++
			default:
				t.Errorf("consume: %v", err)
			}
		}(i + 1)
	}
	wg.Wait()
	if ok != 1 || used != n-1 {
		t.Fatalf("successes %d, replays %d", ok, used)
	}
}

func TestCardFollowupQueue(t *testing.T) {
	s, pool, ctx := liveCards(t)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.approval_card_deliveries`); err != nil {
		t.Fatal(err)
	}
	a, err := s.Issue(ctx, cardIssue(606))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Issue(ctx, cardIssue(607)); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingFollowups(ctx, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	first, _ := s.Lookup(ctx, a)
	if err := s.MarkFollowedUp(ctx, first.ID, "verified"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	pending, err = s.PendingFollowups(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].QueueID != 607 {
		t.Fatalf("pending after mark = %+v, %v", pending, err)
	}
	if err := s.MarkFollowedUp(ctx, first.ID, "verified"); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("marking twice: err = %v, want ErrCardUnknown", err)
	}
	limited, err := s.PendingFollowups(ctx, 0)
	if err != nil || len(limited) != 0 {
		t.Fatalf("limit 0 = %+v, %v", limited, err)
	}
	got, _ := s.Lookup(ctx, a)
	if got.FollowupVerdict != "verified" || got.FollowedUpAt == nil {
		t.Fatalf("followed-up card = %+v", got)
	}
}
