package notify

import (
	"context"
	"testing"
)

// market-14: a Jira integration is a channel row but not an alert
// destination, so a security broadcast skips it instead of logging a
// "no sender" failure on every broadcast.
// No concurrent access tests: one broadcast over a private store.

func TestIsTicketIntegration(t *testing.T) {
	if !IsTicketIntegration("jira") {
		t.Fatal("jira is not a ticket integration")
	}
	for _, typ := range []string{"", "slack", "teams", "opsgenie", "incidentio", "Jira"} {
		if IsTicketIntegration(typ) {
			t.Errorf("%q is a ticket integration", typ)
		}
	}
}

func TestBroadcast_SkipsTicketIntegrations(t *testing.T) {
	slack := newMockSender("slack")
	store := &listingStore{memStore: memStore{channels: map[int]*Channel{
		1: {ID: 1, Type: "jira", Enabled: true},
		2: {ID: 2, Type: "slack", Enabled: true},
	}}}
	d := NewDispatcherWithStore(store, func(string, string, ...any) {})
	d.RegisterSender(slack)
	n, err := d.Broadcast(context.Background(), securityEvent())
	if err != nil || n != 1 {
		t.Fatalf("Broadcast = %d, %v; want 1 (slack only)", n, err)
	}
	if len(store.logged) != 1 || store.logged[0] != EventSecurityBreakGlass+":sent" {
		t.Fatalf("delivery log = %v, want only the slack delivery", store.logged)
	}
}
