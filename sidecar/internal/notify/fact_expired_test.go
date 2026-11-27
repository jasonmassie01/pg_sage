package notify

import (
	"context"
	"strings"
	"testing"
	"time"
)

// facts-08: a confirmed fact that expires (its subject absent past the
// grace, or its date passed) stops binding; operators hear about it as a
// fact_expired event instead of finding out from a missing binding.

func factExpiry() FactExpiry {
	since := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	return FactExpiry{Database: "orders", FactID: 12,
		Title:       "replication slot cdc_orders belongs to debezium",
		ConfirmedBy: "alice@example.com", Reason: "no replication slot matches cdc_orders",
		MissingSince: &since, RestoreDays: 7}
}

func TestFactExpiredEventCarriesTheLostConfirmation(t *testing.T) {
	evt := FactExpiredEvent(factExpiry())
	if evt.Type != "fact_expired" || evt.Severity != "warning" || evt.Resolve {
		t.Fatalf("type %q severity %q resolve %v", evt.Type, evt.Severity, evt.Resolve)
	}
	if !strings.Contains(evt.Subject, "cdc_orders belongs to debezium") {
		t.Fatalf("subject %q", evt.Subject)
	}
	for _, want := range []string{"#12", "alice@example.com", "no replication slot",
		"no longer binds", "7 days"} {
		if !strings.Contains(evt.Body, want) {
			t.Errorf("body %q lacks %q", evt.Body, want)
		}
	}
	if evt.Data["database"] != "orders" || evt.Data["fact_id"] != int64(12) ||
		evt.Data["confirmed_by"] != "alice@example.com" {
		t.Fatalf("data %+v", evt.Data)
	}
	if _, card := FactCardOf(evt); card {
		t.Fatal("an expiry notice must not carry Confirm / Reject buttons")
	}
	// One expiry is one incident; a later absence of the same fact is another.
	again := factExpiry()
	later := again.MissingSince.Add(48 * time.Hour)
	again.MissingSince = &later
	if evt.DedupKey == "" || evt.DedupKey == FactExpiredEvent(again).DedupKey ||
		evt.DedupKey != FactExpiredEvent(factExpiry()).DedupKey {
		t.Fatalf("dedup keys %q / %q", evt.DedupKey, FactExpiredEvent(again).DedupKey)
	}
}

// Zero values: an expiry by date has no absence start and the body says
// nothing about a restore window it does not have.
func TestFactExpiredEventByDate(t *testing.T) {
	x := factExpiry()
	x.MissingSince, x.RestoreDays, x.Reason = nil, 0, "its expiry date passed"
	evt := FactExpiredEvent(x)
	if evt.DedupKey == "" || strings.Contains(evt.Body, "days") ||
		!strings.Contains(evt.Body, "expiry date passed") {
		t.Fatalf("by-date event %+v", evt)
	}
}

func TestFactExpiredIsAValidRoutableEvent(t *testing.T) {
	if !ValidEventTypes["fact_expired"] || EventSeverity["fact_expired"] != "warning" {
		t.Fatalf("fact_expired valid=%v severity=%q", ValidEventTypes["fact_expired"],
			EventSeverity["fact_expired"])
	}
	if !RuleCanFire("fact_expired", DefaultMinSeverity("fact_expired")) ||
		RuleCanFire("fact_expired", "critical") {
		t.Fatal("fact_expired must reach a default rule and not a critical-only one")
	}
	store := &loggingStore{memStore: memStore{
		rules: []Rule{{ID: 1, ChannelID: 3, Event: "fact_expired",
			MinSeverity: DefaultMinSeverity("fact_expired"), Enabled: true}},
		channels: map[int]*Channel{3: {ID: 3, Type: "email", Enabled: true,
			Config: map[string]string{}}},
	}}
	sender := &cardSender{typ: "email"}
	d := NewDispatcherWithStore(store, (&logLines{}).fn)
	d.RegisterSender(sender)
	if err := d.Dispatch(context.Background(), FactExpiredEvent(factExpiry())); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if len(sender.events) != 1 || len(store.logged) != 1 ||
		store.logged[0] != "3:fact_expired:sent" {
		t.Fatalf("sent %d, rows %v; want one fact_expired row", len(sender.events),
			store.logged)
	}
}
