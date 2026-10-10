package notify

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
)

// listingStore is a memStore that can also list its enabled channels and
// records deliveries.
type listingStore struct {
	memStore
	listErr error
	mu      sync.Mutex
	logged  []string
}

func (s *listingStore) EnabledChannelIDs(context.Context) ([]int, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var ids []int
	for id, ch := range s.channels {
		if ch.Enabled {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids, nil
}

func (s *listingStore) LogDelivery(_ context.Context, _ int, evt Event,
	status, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logged = append(s.logged, evt.Type+":"+status)
	return nil
}

func securityEvent() Event {
	return Event{Type: EventSecurityBreakGlass, Severity: "critical",
		Subject: "Break-glass admin login used", Body: "source 203.0.113.9"}
}

func TestBroadcast_ReachesEveryEnabledChannelWithoutRules(t *testing.T) {
	slack := newMockSender("slack")
	pd := newMockSender("pagerduty")
	store := &listingStore{memStore: memStore{channels: map[int]*Channel{
		1: {ID: 1, Type: "slack", Enabled: true},
		2: {ID: 2, Type: "pagerduty", Enabled: true},
		3: {ID: 3, Type: "slack", Enabled: false},
	}}}
	d := NewDispatcherWithStore(store, func(string, string, ...any) {})
	d.RegisterSender(slack)
	d.RegisterSender(pd)
	n, err := d.Broadcast(context.Background(), securityEvent())
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if n != 2 || slack.callCount() != 1 || pd.callCount() != 1 {
		t.Fatalf("delivered %d (slack %d, pagerduty %d), want 2 (1, 1)",
			n, slack.callCount(), pd.callCount())
	}
	if slack.calls[0].Event.Type != EventSecurityBreakGlass {
		t.Fatalf("sent event %q", slack.calls[0].Event.Type)
	}
	if len(store.logged) != 2 {
		t.Fatalf("delivery log = %v, want 2 rows", store.logged)
	}
}

func TestBroadcast_OneFailingChannelDoesNotStopOthers(t *testing.T) {
	bad := newMockSender("slack")
	bad.failNext = true
	good := newMockSender("email")
	store := &listingStore{memStore: memStore{channels: map[int]*Channel{
		1: {ID: 1, Type: "slack", Enabled: true},
		2: {ID: 2, Type: "email", Enabled: true},
	}}}
	d := NewDispatcherWithStore(store, func(string, string, ...any) {})
	d.RegisterSender(bad)
	d.RegisterSender(good)
	n, err := d.Broadcast(context.Background(), securityEvent())
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if n != 1 || good.callCount() != 1 {
		t.Fatalf("delivered %d, good sender %d; want 1, 1", n, good.callCount())
	}
	sort.Strings(store.logged)
	if len(store.logged) != 2 || store.logged[0] != EventSecurityBreakGlass+":error" {
		t.Fatalf("delivery log = %v, want one error and one sent", store.logged)
	}
}

func TestBroadcast_UnknownSenderTypeCountsAsUndelivered(t *testing.T) {
	store := &listingStore{memStore: memStore{channels: map[int]*Channel{
		1: {ID: 1, Type: "carrier-pigeon", Enabled: true},
	}}}
	d := NewDispatcherWithStore(store, func(string, string, ...any) {})
	n, err := d.Broadcast(context.Background(), securityEvent())
	if err != nil || n != 0 {
		t.Fatalf("= (%d, %v), want (0, nil)", n, err)
	}
}

func TestBroadcast_NoChannels(t *testing.T) {
	store := &listingStore{memStore: memStore{channels: map[int]*Channel{}}}
	d := NewDispatcherWithStore(store, func(string, string, ...any) {})
	n, err := d.Broadcast(context.Background(), securityEvent())
	if err != nil || n != 0 {
		t.Fatalf("= (%d, %v), want (0, nil)", n, err)
	}
}

func TestBroadcast_ListErrorPropagates(t *testing.T) {
	store := &listingStore{memStore: memStore{channels: map[int]*Channel{}},
		listErr: errors.New("connection refused")}
	d := NewDispatcherWithStore(store, func(string, string, ...any) {})
	_, err := d.Broadcast(context.Background(), securityEvent())
	if err == nil || !errors.Is(err, store.listErr) {
		t.Fatalf("err = %v, want the list error wrapped", err)
	}
}

func TestBroadcast_StoreWithoutListingIsUnsupported(t *testing.T) {
	d := NewDispatcherWithStore(&memStore{}, func(string, string, ...any) {})
	_, err := d.Broadcast(context.Background(), securityEvent())
	if !errors.Is(err, ErrBroadcastUnsupported) {
		t.Fatalf("err = %v, want ErrBroadcastUnsupported", err)
	}
}

func TestBroadcast_NilDispatcher(t *testing.T) {
	var d *Dispatcher
	if _, err := d.Broadcast(context.Background(), securityEvent()); err == nil {
		t.Fatal("nil dispatcher broadcast succeeded")
	}
}

func TestSecurityBreakGlassEventIsCriticalAndValid(t *testing.T) {
	if !ValidEventTypes[EventSecurityBreakGlass] {
		t.Fatal("security_break_glass is not a valid event type for rules")
	}
	if EventSeverity[EventSecurityBreakGlass] != "critical" {
		t.Fatalf("severity = %q, want critical", EventSeverity[EventSecurityBreakGlass])
	}
}
