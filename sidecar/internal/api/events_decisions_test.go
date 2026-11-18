package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mcp-01b: wait_for_decision waits on this broker. A person's decision is
// an API write, so every successful write publishes a decisions signal at
// once (no statistics flush delay); the dashboard stream never shows it.
// Stopping the broker closes every subscription so no waiter hangs.

func receiveEvent(t *testing.T, ch <-chan Event, d time.Duration) (Event, bool) {
	t.Helper()
	select {
	case evt, open := <-ch:
		return evt, open
	case <-time.After(d):
		t.Fatalf("no event within %s", d)
	}
	return Event{}, false
}

func noEvent(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case evt := <-ch:
		t.Fatalf("unexpected event %+v", evt)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEventBrokerStopClosesEverySubscription(t *testing.T) {
	b := NewEventBroker()
	first, cancelFirst := b.Subscribe()
	second, cancelSecond := b.Subscribe()
	b.Stop()
	for _, ch := range []<-chan Event{first, second} {
		if _, open := receiveEvent(t, ch, time.Second); open {
			t.Fatal("a subscription stayed open after Stop")
		}
	}
	cancelFirst()
	cancelSecond()
	cancelSecond() // idempotent after Stop
	late, cancelLate := b.Subscribe()
	defer cancelLate()
	if _, open := receiveEvent(t, late, time.Second); open {
		t.Fatal("a subscription after Stop must be closed at once")
	}
	if n := b.SubscriberCount(); n != 0 {
		t.Fatalf("subscribers after Stop = %d, want 0", n)
	}
	b.Stop() // safe twice
	b.Publish(Event{Type: EventDecisions})
}

func TestEventBrokerStopsWhenItsContextEnds(t *testing.T) {
	b := NewEventBroker()
	ctx, cancel := context.WithCancel(context.Background())
	b.Start(ctx, nil, time.Hour, time.Hour)
	ch, unsubscribe := b.Subscribe()
	defer unsubscribe()
	cancel()
	if _, open := receiveEvent(t, ch, 2*time.Second); open {
		t.Fatal("the subscription stayed open after the process context ended")
	}
}

func TestDecisionSignalOnSuccessfulWritesOnly(t *testing.T) {
	b := NewEventBroker()
	ch, cancel := b.Subscribe()
	defer cancel()
	status := map[string]int{"/ok": 200, "/created": 201, "/none": 204,
		"/denied": 403, "/bad": 400, "/broken": 500, "/api/v1/mcp": 200, "/mcp": 200,
		"/mcp/x": 200}
	h := decisionSignalMiddleware(b, http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		w.WriteHeader(status[r.URL.Path])
	}))
	cases := []struct {
		method, path string
		signal       bool
	}{
		{http.MethodPost, "/ok", true}, {http.MethodPut, "/created", true},
		{http.MethodPatch, "/ok", true}, {http.MethodDelete, "/none", true},
		{http.MethodGet, "/ok", false}, {http.MethodHead, "/ok", false},
		{http.MethodPost, "/denied", false}, {http.MethodPost, "/bad", false},
		{http.MethodPost, "/broken", false}, {http.MethodPost, "/api/v1/mcp", false},
		{http.MethodPost, "/mcp", false}, {http.MethodPost, "/mcp/x", false},
	}
	for _, c := range cases {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
		if !c.signal {
			noEvent(t, ch)
			continue
		}
		evt, open := receiveEvent(t, ch, time.Second)
		if !open || evt.Type != EventDecisions {
			t.Fatalf("%s %s: event %+v open=%v, want decisions", c.method, c.path, evt,
				open)
		}
	}
}

func TestDecisionSignalWithoutAnyWriteOrBrokerIsSafe(t *testing.T) {
	h := decisionSignalMiddleware(nil, http.HandlerFunc(func(w http.ResponseWriter,
		_ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("response %d %q", rec.Code, rec.Body.String())
	}
}

func TestEventsStreamNeverShowsDecisionSignals(t *testing.T) {
	b := NewEventBroker()
	srv := httptest.NewServer(eventsHandlerWithLifetime(b, 400*time.Millisecond))
	defer srv.Close()
	go func() {
		for b.SubscriberCount() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		b.Publish(Event{Type: EventDecisions})
		b.Publish(Event{Type: EventActions, Database: "orders"})
	}()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body strings.Builder
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		body.Write(buf[:n])
		if readErr != nil {
			break
		}
	}
	text := body.String()
	if !strings.Contains(text, "event: actions") {
		t.Fatalf("the stream lost the actions event: %q", text)
	}
	if strings.Contains(text, "decisions") {
		t.Fatalf("the dashboard stream shows the decisions signal: %q", text)
	}
}
