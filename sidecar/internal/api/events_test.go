package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEventBrokerPublishFansOut(t *testing.T) {
	b := NewEventBroker()
	c1, cancel1 := b.Subscribe()
	defer cancel1()
	c2, cancel2 := b.Subscribe()
	defer cancel2()

	if got := b.SubscriberCount(); got != 2 {
		t.Fatalf("SubscriberCount = %d, want 2", got)
	}

	evt := Event{Type: EventFindings, Database: "main"}
	b.Publish(evt)

	for i, ch := range []<-chan Event{c1, c2} {
		select {
		case got := <-ch:
			if got.Type != EventFindings {
				t.Errorf("sub %d type = %q, want %q",
					i, got.Type, EventFindings)
			}
			if got.Database != "main" {
				t.Errorf("sub %d database = %q, want %q",
					i, got.Database, "main")
			}
			if got.Ts.IsZero() {
				t.Errorf("sub %d Ts is zero — Publish should stamp it",
					i)
			}
		case <-time.After(500 * time.Millisecond):
			t.Errorf("sub %d: no event received within 500ms", i)
		}
	}
}

func TestEventBrokerSubscribeCancelRemoves(t *testing.T) {
	b := NewEventBroker()
	_, cancel := b.Subscribe()
	if got := b.SubscriberCount(); got != 1 {
		t.Fatalf("after subscribe: count = %d, want 1", got)
	}
	cancel()
	if got := b.SubscriberCount(); got != 0 {
		t.Fatalf("after cancel: count = %d, want 0", got)
	}
	// Second cancel must be a no-op (not double-close).
	cancel()
	if got := b.SubscriberCount(); got != 0 {
		t.Fatalf("after double-cancel: count = %d, want 0", got)
	}
}

func TestEventBrokerPublishDropsSlowSubscriber(t *testing.T) {
	b := NewEventBroker()
	_, cancel := b.Subscribe()
	defer cancel()

	// Channel buffer is 32; publish 100 events without reading.
	// The broker must never block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish(Event{Type: EventFindings})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatalf("Publish blocked on a slow subscriber — broker stalled")
	}
}

func TestEventBrokerStartIdempotent(t *testing.T) {
	b := NewEventBroker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// First Start should set the flag; subsequent calls are no-ops.
	b.Start(ctx, nil, 10*time.Millisecond, 10*time.Millisecond)
	if !b.started.Load() {
		t.Fatal("started flag not set after first Start")
	}
	b.Start(ctx, nil, 10*time.Millisecond, 10*time.Millisecond)
	// No panic / goroutine leak is the assertion here.
	b.Stop()
	// Stop twice is safe.
	b.Stop()
}

func TestEventBrokerPollOnceNilManagerIsSafe(t *testing.T) {
	b := NewEventBroker()
	// No subscriber, no pool, nil manager — must not panic.
	b.pollOnce(context.Background(), nil, map[string]map[EventType]lastSeen{})
}

func TestEventBrokerPollOncePublishesActionQueueChanges(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	mgr := phase2MgrWithPool(pool)

	b := NewEventBroker()
	ch, cancel := b.Subscribe()
	defer cancel()

	state := map[string]map[EventType]lastSeen{}
	b.pollOnce(ctx, mgr, state)
	assertNoEvent(t, ch)

	_, err := pool.Exec(ctx,
		`INSERT INTO sage.action_queue
		 (finding_id, proposed_sql, action_risk)
		 VALUES (42, 'CREATE INDEX idx_sse_queue ON t (c)', 'safe')`)
	if err != nil {
		t.Fatalf("insert queued action: %v", err)
	}

	b.pollOnce(ctx, mgr, state)

	select {
	case evt := <-ch:
		if evt.Type != EventActions {
			t.Fatalf("event type = %q, want %q",
				evt.Type, EventActions)
		}
		if evt.Database != "testdb" {
			t.Fatalf("event database = %q, want testdb",
				evt.Database)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no actions event after action_queue change")
	}
}

func TestEventBrokerHeartbeatLoopStopsOnCtx(t *testing.T) {
	b := NewEventBroker()
	ctx, cancel := context.WithCancel(context.Background())
	ch, sub := b.Subscribe()
	defer sub()

	done := make(chan struct{})
	go func() {
		b.heartbeatLoop(ctx, 20*time.Millisecond)
		close(done)
	}()

	// Confirm at least one heartbeat lands.
	select {
	case evt := <-ch:
		if evt.Type != EventHeartbeat {
			t.Fatalf("first event type = %q, want heartbeat", evt.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no heartbeat within 500ms")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("heartbeatLoop did not exit after ctx cancel")
	}
}

func TestEventBrokerPublishStampsTimestamp(t *testing.T) {
	b := NewEventBroker()
	ch, cancel := b.Subscribe()
	defer cancel()

	before := time.Now().UTC()
	b.Publish(Event{Type: EventActions}) // no Ts set
	evt := <-ch
	after := time.Now().UTC()

	if evt.Ts.Before(before) || evt.Ts.After(after.Add(time.Second)) {
		t.Fatalf("Ts = %v, want between %v and %v",
			evt.Ts, before, after)
	}
}

func TestEventBrokerPublishPreservesExplicitTimestamp(t *testing.T) {
	b := NewEventBroker()
	ch, cancel := b.Subscribe()
	defer cancel()

	explicit := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	b.Publish(Event{Type: EventHealth, Ts: explicit})
	evt := <-ch
	if !evt.Ts.Equal(explicit) {
		t.Fatalf("Ts = %v, want %v (explicit, not overwritten)",
			evt.Ts, explicit)
	}
}

func TestEventsHandlerStreamsSubscribedEvent(t *testing.T) {
	b := NewEventBroker()
	h := eventsHandler(b)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	ctx, cancelReq := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	// Run handler in a goroutine; stream stays open until we cancel.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h(rec, req)
	}()

	// Give the handler a moment to register its subscriber.
	deadline := time.Now().Add(500 * time.Millisecond)
	for b.SubscriberCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if b.SubscriberCount() == 0 {
		cancelReq()
		wg.Wait()
		t.Fatal("handler never subscribed")
	}

	b.Publish(Event{Type: EventFindings, Database: "primary"})
	time.Sleep(30 * time.Millisecond)
	cancelReq()
	wg.Wait()

	body := rec.Body.String()
	if !strings.HasPrefix(body, "retry: 3000") {
		t.Errorf("stream did not begin with retry hint; got: %q",
			firstLine(body))
	}
	if !strings.Contains(body, "event: findings") {
		t.Errorf("stream missing event: findings frame; got: %s", body)
	}
	if !strings.Contains(body, `"database":"primary"`) {
		t.Errorf("stream missing payload database; got: %s", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	if sc.Scan() {
		return sc.Text()
	}
	return ""
}

func assertNoEvent(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case evt := <-ch:
		t.Fatalf("unexpected baseline event: %+v", evt)
	case <-time.After(50 * time.Millisecond):
	}
}
