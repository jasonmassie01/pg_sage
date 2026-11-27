package approvalcard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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

// queueDispatch counts the re-sends of each queue item and fails the ones
// in fail; delay widens the window two racing runs overlap in.
type queueDispatch struct {
	mu     sync.Mutex
	counts map[int]int
	fail   map[int]bool
	delay  time.Duration
}

func (q *queueDispatch) Dispatch(_ context.Context, evt notify.Event) error {
	id, _ := evt.Data["queue_id"].(int)
	time.Sleep(q.delay)
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.counts == nil {
		q.counts = map[int]int{}
	}
	q.counts[id]++
	if q.fail[id] {
		return fmt.Errorf("slack 503 for item %d", id)
	}
	return nil
}

func (q *queueDispatch) count(id int) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.counts[id]
}

// clearSnoozes ends every snooze other tests left behind, so a run sees
// only this test's items.
func clearSnoozes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET snoozed_until = NULL
		WHERE snoozed_until IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
}

// endedSnoozes queues n items whose snooze ran out a second ago.
func endedSnoozes(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) []int {
	t.Helper()
	ids := make([]int, n)
	for i := range ids {
		ids[i], _ = queueOptimizerIndex(t, ctx, pool)
		endSnooze(t, ctx, pool, ids[i])
	}
	return ids
}

func endSnooze(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET snoozed_until =
		now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// snoozeState reads whether the item's snooze is cleared, and whether it
// is held for the 5-minute retry.
func snoozeState(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	id int) (cleared, retry bool) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT snoozed_until IS NULL,
		COALESCE(snoozed_until > now() + interval '4 minutes'
		     AND snoozed_until <= now() + interval '5 minutes', false)
		FROM sage.action_queue WHERE id = $1`, id).Scan(&cleared, &retry); err != nil {
		t.Fatal(err)
	}
	return cleared, retry
}

func TestRenotifySnoozedContinuesPastAFailedResend(t *testing.T) {
	pool, ctx := livePool(t)
	clearSnoozes(t, ctx, pool)
	ids := endedSnoozes(t, ctx, pool, 3)
	d := &queueDispatch{fail: map[int]bool{ids[1]: true}}
	l := Loader{Pool: pool, Database: "orders"}
	n, err := RenotifySnoozed(ctx, l, d)
	if n != 2 || err == nil {
		t.Fatalf("renotify = %d, %v; want 2 sent and an error", n, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, fmt.Sprintf("card %d:", ids[1])) ||
		strings.Contains(msg, fmt.Sprintf("card %d:", ids[0])) ||
		strings.Contains(msg, fmt.Sprintf("card %d:", ids[2])) ||
		!strings.Contains(msg, "slack 503") {
		t.Fatalf("error %q must name queue item %d only", msg, ids[1])
	}
	for _, id := range ids {
		if d.count(id) != 1 {
			t.Fatalf("item %d dispatched %d times, want 1", id, d.count(id))
		}
	}
	for i, id := range []int{ids[0], ids[2]} {
		if cleared, _ := snoozeState(t, ctx, pool, id); !cleared {
			t.Fatalf("sent item %d (#%d) kept its snooze", id, i)
		}
	}
	if cleared, retry := snoozeState(t, ctx, pool, ids[1]); cleared || !retry {
		t.Fatalf("failed item: cleared %v, 5-minute retry %v", cleared, retry)
	}
	// The retry waits its 5 minutes: an immediate run sends nothing.
	if n, err := RenotifySnoozed(ctx, l, d); n != 0 || err != nil || d.count(ids[1]) != 1 {
		t.Fatalf("immediate rerun = %d, %v, item sent %d times", n, err, d.count(ids[1]))
	}
	endSnooze(t, ctx, pool, ids[1])
	d.fail = nil
	if n, err := RenotifySnoozed(ctx, l, d); n != 1 || err != nil || d.count(ids[1]) != 2 {
		t.Fatalf("retry = %d, %v, item sent %d times", n, err, d.count(ids[1]))
	}
	if cleared, _ := snoozeState(t, ctx, pool, ids[1]); !cleared {
		t.Fatal("the retried item kept its snooze")
	}
}

func TestRenotifySnoozedRacingRunsSendEachOnce(t *testing.T) {
	pool, ctx := livePool(t)
	clearSnoozes(t, ctx, pool)
	ids := endedSnoozes(t, ctx, pool, 3)
	d := &queueDispatch{delay: 30 * time.Millisecond}
	l := Loader{Pool: pool, Database: "orders"}
	const runs = 4
	var wg sync.WaitGroup
	sent := make([]int, runs)
	errs := make([]error, runs)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sent[i], errs[i] = RenotifySnoozed(ctx, l, d)
		}(i)
	}
	wg.Wait()
	total := 0
	for i := 0; i < runs; i++ {
		if errs[i] != nil {
			t.Fatalf("run %d: %v", i, errs[i])
		}
		total += sent[i]
	}
	for _, id := range ids {
		if d.count(id) != 1 {
			t.Fatalf("item %d dispatched %d times by racing runs, want 1", id, d.count(id))
		}
	}
	if total != len(ids) {
		t.Fatalf("runs report %d sent, want %d", total, len(ids))
	}
}

func TestRenotifySnoozedNothingDueAndNoDispatcher(t *testing.T) {
	pool, ctx := livePool(t)
	clearSnoozes(t, ctx, pool)
	l := Loader{Pool: pool, Database: "orders"}
	d := &queueDispatch{}
	if n, err := RenotifySnoozed(ctx, l, d); n != 0 || err != nil || len(d.counts) != 0 {
		t.Fatalf("nothing due = %d, %v, dispatched %v", n, err, d.counts)
	}
	id := endedSnoozes(t, ctx, pool, 1)[0]
	t.Cleanup(func() { clearSnoozes(t, context.Background(), pool) })
	// A typed nil notifier (notifications off) is no dispatcher either.
	for i, none := range []Dispatcher{nil, NewNotifier(nil, l)} {
		n, err := RenotifySnoozed(ctx, l, none)
		if n != 0 || err == nil || !strings.Contains(err.Error(), "dispatcher") {
			t.Fatalf("dispatcher #%d: %d, %v", i, n, err)
		}
		if cleared, retry := snoozeState(t, ctx, pool, id); cleared || retry {
			t.Fatalf("dispatcher #%d touched the snooze: cleared %v, retry %v", i,
				cleared, retry)
		}
	}
	if n, err := RenotifySnoozed(ctx, Loader{}, d); n != 0 || err == nil {
		t.Fatalf("no pool: %d, %v", n, err)
	}
}
