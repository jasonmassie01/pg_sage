package notify

import (
	"sort"
	"testing"
)

// alerting-06 M4: one constructor registers every built-in sender, so the
// API router and the runtime cannot drift apart again.
// No concurrent access tests: construction only, no shared state.

func TestNewDefaultDispatcher_RegistersEveryBuiltInSender(t *testing.T) {
	d := NewDefaultDispatcher(&fakeRuleStore{}, TargetPolicy{}, nil)
	var got []string
	for typ := range d.senders {
		got = append(got, typ)
	}
	sort.Strings(got)
	want := []string{"email", "incidentio", "opsgenie", "pagerduty", "slack", "teams",
		"telegram", "webhook"}
	if len(got) != len(want) {
		t.Fatalf("senders = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("senders = %v, want %v", got, want)
		}
	}
}

func TestNewDefaultDispatcher_NilLogFnIsSafe(t *testing.T) {
	d := NewDefaultDispatcher(&fakeRuleStore{}, TargetPolicy{}, nil)
	if d.logFn == nil {
		t.Fatal("nil logFn kept; a delivery failure would panic")
	}
	d.logFn("ERROR", "x %d", 1)
}

func TestNewDefaultDispatcher_SendersShareThePolicy(t *testing.T) {
	d := NewDefaultDispatcher(&fakeRuleStore{}, TargetPolicy{AllowPrivate: true}, nil)
	wh, ok := d.senders["webhook"].(*WebhookSender)
	if !ok || !wh.policy.AllowPrivate {
		t.Fatalf("webhook sender = %#v, want the dispatcher's policy", d.senders["webhook"])
	}
	sl, ok := d.senders["slack"].(*SlackSender)
	if !ok || !sl.policy.AllowPrivate {
		t.Fatalf("slack sender = %#v, want the dispatcher's policy", d.senders["slack"])
	}
}
