package approvalcard

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/chatops"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

// The notifier turns the executor's approval_needed event for a queued
// item into a card (body and card reference); everything else passes
// through unchanged.

type captureDispatch struct {
	mu     sync.Mutex
	events []notify.Event
	err    error
}

func (c *captureDispatch) Dispatch(_ context.Context, evt notify.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, evt)
	return c.err
}

func (c *captureDispatch) last(t *testing.T) notify.Event {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) == 0 {
		t.Fatal("nothing dispatched")
	}
	return c.events[len(c.events)-1]
}

func TestNotifierEnrichesQueuedApprovals(t *testing.T) {
	pool, ctx := livePool(t)
	queueID, _ := queueOptimizerIndex(t, ctx, pool)
	inner := &captureDispatch{}
	n := NewNotifier(inner, Loader{Pool: pool, Database: "orders", TrustLevel: "advisory"})
	evt := notify.QueuedApprovalEvent("Index recommendation for public.orders",
		"CREATE INDEX ...", "orders", "moderate", queueID)
	if err := n.Dispatch(ctx, evt); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got := inner.last(t)
	ref, ok := notify.ApprovalCardOf(got)
	if !ok || ref.QueueID != queueID || ref.Database != "orders" || ref.CardHash == "" ||
		ref.Title != "Index recommendation for public.orders" ||
		!strings.Contains(ref.Summary, "42.5%") || !ref.ExpiresAt.After(time.Now()) {
		t.Fatalf("card ref = %+v, %v", ref, ok)
	}
	for _, want := range []string{"Why it needs you", "Evidence", "Rollback",
		"seq scans filter by customer_id"} {
		if !strings.Contains(got.Body, want) {
			t.Fatalf("body lacks %q:\n%s", want, got.Body)
		}
	}
}

func TestNotifierPassesOtherEventsThrough(t *testing.T) {
	pool, ctx := livePool(t)
	inner := &captureDispatch{}
	n := NewNotifier(inner, Loader{Pool: pool, Database: "orders"})
	plain := notify.ActionExecutedEvent("t", "ANALYZE t", "orders")
	legacy := notify.ApprovalNeededEvent("t", "ANALYZE t", "orders", "safe")
	missing := notify.QueuedApprovalEvent("t", "ANALYZE t", "orders", "safe", 987654321)
	for _, evt := range []notify.Event{plain, legacy, missing} {
		if err := n.Dispatch(ctx, evt); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		got := inner.last(t)
		if _, ok := notify.ApprovalCardOf(got); ok || got.Body != evt.Body {
			t.Fatalf("event %s was changed: %+v", evt.Type, got)
		}
	}
	inner.err = errors.New("rules unavailable")
	if err := n.Dispatch(ctx, plain); err == nil {
		t.Fatal("the inner dispatcher's error was swallowed")
	}
	if NewNotifier(nil, Loader{Pool: pool}) != nil {
		t.Fatal("a notifier without a dispatcher must be nil")
	}
}

func TestRenotifySnoozedOncePerExpiry(t *testing.T) {
	pool, ctx := livePool(t)
	queueID, _ := queueOptimizerIndex(t, ctx, pool)
	as := store.NewActionStore(pool)
	if err := as.Snooze(ctx, queueID, 3, time.Now().Add(time.Hour), "later"); err != nil {
		t.Fatal(err)
	}
	inner := &captureDispatch{}
	l := Loader{Pool: pool, Database: "orders"}
	n, err := RenotifySnoozed(ctx, l, NewNotifier(inner, l))
	if err != nil || n != 0 {
		t.Fatalf("a running snooze re-notified: %d, %v", n, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET snoozed_until =
		now() - interval '1 second' WHERE id = $1`, queueID); err != nil {
		t.Fatal(err)
	}
	n, err = RenotifySnoozed(ctx, l, NewNotifier(inner, l))
	if err != nil || n < 1 {
		t.Fatalf("renotify = %d, %v", n, err)
	}
	found := false
	for _, e := range inner.events {
		if ref, ok := notify.ApprovalCardOf(e); ok && ref.QueueID == queueID {
			found = true
		}
	}
	if !found {
		t.Fatalf("no card re-sent for %d: %+v", queueID, inner.events)
	}
	var until *time.Time
	_ = pool.QueryRow(ctx, `SELECT snoozed_until FROM sage.action_queue WHERE id = $1`,
		queueID).Scan(&until)
	if until != nil {
		t.Fatalf("snooze not cleared: %v", until)
	}
	before := len(inner.events)
	if _, err := RenotifySnoozed(ctx, l, NewNotifier(inner, l)); err != nil {
		t.Fatal(err)
	}
	for _, e := range inner.events[before:] {
		if ref, ok := notify.ApprovalCardOf(e); ok && ref.QueueID == queueID {
			t.Fatal("re-notified twice for one snooze")
		}
	}
}

func TestTokenIssuerMintsChannelScopedTokens(t *testing.T) {
	pool, ctx := livePool(t)
	cards := chatops.NewCardStore(pool)
	iss := TokenIssuer{Store: cards, TTL: time.Hour}
	ref := notify.CardRef{Database: "orders", QueueID: 5, CardHash: "h", Title: "t",
		Summary: "s", ExpiresAt: time.Now().Add(48 * time.Hour)}
	token, err := iss.Issue(ctx, notify.Channel{ID: 3, Type: "slack"}, ref)
	if err != nil || !chatops.ValidCardToken(token) {
		t.Fatalf("issue = %q, %v", token, err)
	}
	d, err := cards.Lookup(ctx, token)
	if err != nil || d.ChannelID != 3 || d.QueueID != 5 || d.CardHash != "h" {
		t.Fatalf("delivery = %+v, %v", d, err)
	}
	if d.ExpiresAt.After(time.Now().Add(61 * time.Minute)) {
		t.Fatalf("token outlives the card TTL: %v", d.ExpiresAt)
	}
	short := ref
	short.ExpiresAt = time.Now().Add(10 * time.Minute)
	token, err = iss.Issue(ctx, notify.Channel{ID: 3}, short)
	if err != nil {
		t.Fatal(err)
	}
	d, _ = cards.Lookup(ctx, token)
	if d.ExpiresAt.After(short.ExpiresAt.Add(time.Second)) {
		t.Fatalf("token outlives the queue item: %v > %v", d.ExpiresAt, short.ExpiresAt)
	}
	if err := iss.Revoke(ctx, token); err != nil {
		t.Fatal(err)
	}
	if _, err := cards.Lookup(ctx, token); !errors.Is(err, chatops.ErrCardUnknown) {
		t.Fatalf("revoked: %v", err)
	}
	expired := ref
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := iss.Issue(ctx, notify.Channel{ID: 3}, expired); err == nil {
		t.Fatal("issued a token for an expired card")
	}
}
