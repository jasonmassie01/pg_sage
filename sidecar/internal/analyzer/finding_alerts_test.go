package analyzer

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/notify"
)

// alerting-06 M3: with a dispatcher that takes finding alerts, every
// opened, reopened or escalated finding is sent as finding_opened with its
// severity, and every cleared one as finding_resolved. A dispatcher that
// does not take them (the old synchronous one) gets none.
// No concurrent access tests: called only from the cycle goroutine.

type alertingDispatcher struct{ mockDispatcher }

func (*alertingDispatcher) AlertsFindings() bool { return true }

func TestDispatchFindingAlerts_EverySeverity(t *testing.T) {
	a, _, _ := resolveNotifyAnalyzer(nil)
	d := &alertingDispatcher{}
	a.WithDispatcher(d)
	a.dispatchFindingAlerts(context.Background(), UpsertResult{
		Opened:    []Finding{{Category: "bloat", ObjectIdentifier: "t1", Severity: "info"}},
		Reopened:  []Finding{{Category: "bloat", ObjectIdentifier: "t2", Severity: "warning"}},
		Escalated: []Finding{{Category: "xid", ObjectIdentifier: "db", Severity: "critical"}},
	})
	if d.count() != 3 {
		t.Fatalf("dispatched %d events, want 3", d.count())
	}
	want := map[string]string{"finding:orders:bloat:t1": "info",
		"finding:orders:bloat:t2": "warning", "finding:orders:xid:db": "critical"}
	for _, evt := range d.events {
		if evt.Type != "finding_opened" || want[evt.DedupKey] != evt.Severity {
			t.Errorf("event %s %s %s not expected", evt.Type, evt.DedupKey, evt.Severity)
		}
	}
}

func TestDispatchFindingResolved_EveryClearedFinding(t *testing.T) {
	a, _, _ := resolveNotifyAnalyzer(nil)
	d := &alertingDispatcher{}
	a.WithDispatcher(d)
	a.dispatchFindingResolved(context.Background(), []ClearedFinding{
		{Category: "bloat", ObjectIdentifier: "t1", Severity: "info", Title: "b"},
		{Category: "xid", ObjectIdentifier: "db", Severity: "critical", Title: "x"},
	})
	if d.count() != 2 {
		t.Fatalf("dispatched %d events, want 2", d.count())
	}
	for _, evt := range d.events {
		if evt.Type != "finding_resolved" || !evt.Resolve {
			t.Errorf("event = %+v, want a finding_resolved resolve", evt)
		}
	}
	if notify.EventDedupKey(d.events[0]) != "finding:orders:bloat:t1" {
		t.Errorf("key = %q", notify.EventDedupKey(d.events[0]))
	}
}

func TestFindingAlerts_SkippedWithoutAnAlertingDispatcher(t *testing.T) {
	a, d, _ := resolveNotifyAnalyzer(nil)
	a.dispatchFindingAlerts(context.Background(), UpsertResult{
		Opened: []Finding{{Category: "bloat", ObjectIdentifier: "t1", Severity: "warning"}}})
	a.dispatchFindingResolved(context.Background(), []ClearedFinding{
		{Category: "bloat", ObjectIdentifier: "t1", Severity: "warning"}})
	if d.count() != 0 {
		t.Fatalf("dispatched %d events to a non-alerting dispatcher", d.count())
	}
	a.WithDispatcher(nil)
	a.dispatchFindingAlerts(context.Background(), UpsertResult{})
}
