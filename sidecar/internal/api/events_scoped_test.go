package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
)

// apiauth-08b: the SSE stream snapshots the caller's visible databases at
// connect. Events of other databases are dropped; events with no database
// pass only when their type is fleet-wide with no per-database payload
// (the heartbeat). A revoked grant applies at the next reconnect, at most
// one stream lifetime (45 s) later. The poller (events_poll.go) stamps
// every resource event with its database, so it is filtered the same way.

func TestEventsScoped_EventVisibleRules(t *testing.T) {
	visible := map[string]bool{"billing": true}
	cases := []struct {
		name   string
		evt    Event
		scoped bool
		want   bool
	}{
		{"unscoped sees other database", Event{Type: EventFindings, Database: "search"},
			false, true},
		{"unscoped sees database-less", Event{Type: EventFindings}, false, true},
		{"scoped sees granted", Event{Type: EventActions, Database: "billing"}, true, true},
		{"scoped drops other", Event{Type: EventActions, Database: "search"}, true, false},
		{"scoped keeps heartbeat", Event{Type: EventHeartbeat}, true, true},
		{"scoped drops database-less resource", Event{Type: EventHealth}, true, false},
		{"scoped drops unknown type", Event{Type: "fleet_secret"}, true, false},
		{"scoped heartbeat naming another database", Event{Type: EventHeartbeat,
			Database: "search"}, true, false},
	}
	for _, tc := range cases {
		if got := eventVisible(tc.evt, visible, tc.scoped); got != tc.want {
			t.Errorf("%s: eventVisible = %v, want %v", tc.name, got, tc.want)
		}
	}
	if eventVisible(Event{Type: EventFindings, Database: "billing"}, nil, true) {
		t.Error("a scoped stream with a nil set must see no database")
	}
}

type scopedStream struct {
	rec    *syncRecorder
	cancel context.CancelFunc
	done   chan struct{}
}

func openScopedStream(t *testing.T, h http.Handler, b *EventBroker, user *auth.User,
	subscribers int) *scopedStream {
	t.Helper()
	req := withUser(httptest.NewRequest(http.MethodGet, "/api/v1/events", nil), user)
	ctx, cancel := context.WithCancel(req.Context())
	s := &scopedStream{rec: &syncRecorder{rec: httptest.NewRecorder()}, cancel: cancel,
		done: make(chan struct{})}
	go func() {
		defer close(s.done)
		h.ServeHTTP(s.rec, req.WithContext(ctx))
	}()
	if !waitFor(5*time.Second, func() bool { return b.SubscriberCount() >= subscribers }) {
		cancel()
		<-s.done
		t.Fatalf("stream never subscribed: %s", s.rec.body())
	}
	return s
}

func (s *scopedStream) waitFor(t *testing.T, marker string) string {
	t.Helper()
	if !waitFor(5*time.Second, func() bool { return strings.Contains(s.rec.body(), marker) }) {
		t.Fatalf("stream never delivered %q; got %s", marker, s.rec.body())
	}
	return s.rec.body()
}

func (s *scopedStream) close() {
	s.cancel()
	<-s.done
}

func scopedEventsMux(src DatabaseGrantSource, b *EventBroker,
	lifetime time.Duration) http.Handler {
	mux := http.NewServeMux()
	routeRead(mux, "GET /api/v1/events", gateViewer, readStream,
		eventsHandlerWithLifetime(b, lifetime))
	return withDatabaseAuthz(&databaseAuthz{grants: src}, mux)
}

func publishMarked(b *EventBroker, typ EventType, database, marker string) {
	b.Publish(Event{Type: typ, Database: database, Payload: map[string]any{"m": marker}})
}

func TestEventsScoped_StreamDropsOtherDatabases(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	b := NewEventBroker()
	h := scopedEventsMux(src, b, time.Minute)
	s := openScopedStream(t, h, b, dbViewer(7), 1)
	defer s.close()
	publishMarked(b, EventFindings, "search", "leak-search")
	publishMarked(b, EventHealth, "", "leak-fleet")
	b.Publish(Event{Type: EventHeartbeat})
	publishMarked(b, EventFindings, "billing", "ok-billing")
	body := s.waitFor(t, "ok-billing")
	for _, leak := range []string{"leak-search", "leak-fleet", `"database":"search"`} {
		if strings.Contains(body, leak) {
			t.Fatalf("scoped stream delivered %q: %s", leak, body)
		}
	}
	if !strings.Contains(body, "event: heartbeat") {
		t.Fatalf("scoped stream dropped the heartbeat: %s", body)
	}
}

func TestEventsScoped_UnscopedStreamUnchanged(t *testing.T) {
	src := &dbAuthzSource{}
	b := NewEventBroker()
	h := scopedEventsMux(src, b, time.Minute)
	s := openScopedStream(t, h, b, dbViewer(8), 1)
	defer s.close()
	publishMarked(b, EventFindings, "search", "m-search")
	publishMarked(b, EventHealth, "", "m-fleet")
	publishMarked(b, EventFindings, "billing", "m-billing")
	body := s.waitFor(t, "m-billing")
	if !strings.Contains(body, "m-search") || !strings.Contains(body, "m-fleet") {
		t.Fatalf("unscoped stream lost events: %s", body)
	}
}

func TestEventsScoped_GrantStoreFailureRefusesStream(t *testing.T) {
	src := &dbAuthzSource{err: errors.New("pool closed")}
	b := NewEventBroker()
	h := scopedEventsMux(src, b, time.Minute)
	w := readGet(h, "/api/v1/events", dbViewer(7))
	requireCode(t, "stream with grant store down", w, http.StatusServiceUnavailable,
		"grant_store_unavailable")
	if b.SubscriberCount() != 0 {
		t.Fatal("a refused stream subscribed to the broker")
	}
}

func TestEventsScoped_RevokeAppliesAtReconnect(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(7, map[string]string{"billing": auth.RoleViewer, "search": auth.RoleViewer})
	b := NewEventBroker()
	lifetime := 300 * time.Millisecond // stands in for the 45 s stream lifetime
	h := scopedEventsMux(src, b, lifetime)
	first := openScopedStream(t, h, b, dbViewer(7), 1)
	src.set(7, map[string]string{"billing": auth.RoleViewer})
	publishMarked(b, EventFindings, "search", "before-reconnect")
	first.waitFor(t, "before-reconnect") // the connect-time snapshot still holds
	select {
	case <-first.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived its lifetime")
	}
	second := openScopedStream(t, h, b, dbViewer(7), 1)
	defer second.close()
	publishMarked(b, EventFindings, "search", "after-reconnect")
	publishMarked(b, EventFindings, "billing", "billing-after")
	body := second.waitFor(t, "billing-after")
	if strings.Contains(body, "after-reconnect") {
		t.Fatalf("revoked database still streamed after reconnect: %s", body)
	}
}

func TestEventsScoped_ConcurrentStreamsOfTwoUsers(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(1, map[string]string{"billing": auth.RoleViewer})
	src.set(2, map[string]string{"search": auth.RoleViewer})
	b := NewEventBroker()
	h := scopedEventsMux(src, b, time.Minute)
	one := openScopedStream(t, h, b, dbViewer(1), 1)
	defer one.close()
	two := openScopedStream(t, h, b, dbViewer(2), 2)
	defer two.close()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); publishMarked(b, EventActions, "billing", "b-mid") }()
		go func() { defer wg.Done(); publishMarked(b, EventActions, "search", "s-mid") }()
	}
	wg.Wait()
	publishMarked(b, EventActions, "billing", "b-end")
	publishMarked(b, EventActions, "search", "s-end")
	if body := one.waitFor(t, "b-end"); strings.Contains(body, "s-mid") ||
		strings.Contains(body, "s-end") {
		t.Fatalf("billing stream saw search: %s", body)
	}
	if body := two.waitFor(t, "s-end"); strings.Contains(body, "b-mid") ||
		strings.Contains(body, "b-end") {
		t.Fatalf("search stream saw billing: %s", body)
	}
}
