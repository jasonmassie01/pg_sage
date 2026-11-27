package notify

import (
	"strings"
	"testing"
)

// executor-04b: a fleet canary a sidecar restart interrupted raises one
// warning canary_interrupted event naming the databases it was changing.

// No concurrent access tests: the constructor is a pure function.

func TestCanaryInterruptedEventNamesTheRunAndItsTargets(t *testing.T) {
	evt := CanaryInterruptedEvent(CanaryInterrupted{RunID: "canary-abc",
		Source: "orders", Changing: []CanaryTargetAction{{Database: "orders_eu",
			ActionLogID: 101}, {Database: "orders_us"}}})
	if evt.Type != "canary_interrupted" || evt.Severity != "warning" {
		t.Fatalf("event type/severity = %s/%s", evt.Type, evt.Severity)
	}
	if evt.DedupKey != "canary_interrupted:canary-abc" || EventDedupKey(evt) != evt.DedupKey {
		t.Fatalf("DedupKey = %q / %q", evt.DedupKey, EventDedupKey(evt))
	}
	for _, want := range []string{"canary-abc", "orders", "orders_eu (action 101)",
		"orders_us (no action recorded yet)", "not resumed"} {
		if !strings.Contains(evt.Subject+"\n"+evt.Body, want) {
			t.Fatalf("subject/body lack %q: %q / %q", want, evt.Subject, evt.Body)
		}
	}
	dbs, ok := evt.Data["changing"].([]string)
	if evt.Data["run_id"] != "canary-abc" || evt.Data["source_database"] != "orders" ||
		!ok || strings.Join(dbs, ",") != "orders_eu,orders_us" {
		t.Fatalf("data = %v", evt.Data)
	}
}

// Nil/empty: a run interrupted before it changed any database says so.
func TestCanaryInterruptedEventWithNothingInFlight(t *testing.T) {
	evt := CanaryInterruptedEvent(CanaryInterrupted{RunID: "canary-x", Source: "src"})
	if !strings.Contains(evt.Body, "No database was being changed") {
		t.Fatalf("body %q, want no database named", evt.Body)
	}
	if dbs, ok := evt.Data["changing"].([]string); !ok || len(dbs) != 0 {
		t.Fatalf("changing = %#v, want an empty list", evt.Data["changing"])
	}
}

func TestCanaryInterruptedIsARoutableWarningEvent(t *testing.T) {
	if !ValidEventTypes["canary_interrupted"] {
		t.Fatal("canary_interrupted is not a valid rule event")
	}
	if EventSeverity["canary_interrupted"] != "warning" ||
		DefaultMinSeverity("canary_interrupted") != "warning" {
		t.Fatalf("severity = %q, want warning", EventSeverity["canary_interrupted"])
	}
	if !RuleCanFire("canary_interrupted", "warning") ||
		RuleCanFire("canary_interrupted", "critical") {
		t.Fatal("rule severities for canary_interrupted are wrong")
	}
}

// An interrupted canary rolls nothing back: the databases it changed with
// a verdict are named as keeping the change, apart from those in flight.
func TestCanaryInterruptedEventNamesTheDatabasesThatKeepTheChange(t *testing.T) {
	evt := CanaryInterruptedEvent(CanaryInterrupted{RunID: "canary-k", Source: "src",
		Kept: []CanaryTargetAction{{Database: "eu", ActionLogID: 5},
			{Database: "us", ActionLogID: 6}}})
	if !strings.Contains(evt.Body, "No database was being changed") ||
		!strings.Contains(evt.Body, "kept (nothing was rolled back): eu (action 5), "+
			"us (action 6).") {
		t.Fatalf("body %q, want nothing in flight and eu, us kept", evt.Body)
	}
	kept, ok := evt.Data["kept"].([]string)
	if !ok || strings.Join(kept, ",") != "eu,us" {
		t.Fatalf("kept = %#v, want [eu us]", evt.Data["kept"])
	}
	none := CanaryInterruptedEvent(CanaryInterrupted{RunID: "canary-n", Source: "src"})
	if strings.Contains(none.Body, "kept") {
		t.Fatalf("a run that changed nothing names kept databases: %q", none.Body)
	}
}
