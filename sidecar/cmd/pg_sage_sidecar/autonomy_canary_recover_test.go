package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/rollout"
)

// executor-04b: each canary the startup sweep interrupted becomes one
// canary_interrupted notification naming what it was changing.

// No concurrent access tests: the conversion is a pure function.

func TestCanaryInterruptedEventsOnePerRun(t *testing.T) {
	events := canaryInterruptedEvents([]rollout.InterruptedRun{
		{Run: rollout.RunRecord{EvidenceID: "canary-1", SourceInstance: "src"},
			Changing: []rollout.InstanceRecord{{InstanceID: "a", ActionLogID: 9,
				Status: rollout.StatusInterrupted}}},
		{Run: rollout.RunRecord{EvidenceID: "canary-2", SourceInstance: "src"}},
	})
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Type != "canary_interrupted" || events[0].DedupKey !=
		"canary_interrupted:canary-1" || !strings.Contains(events[0].Body, "a (action 9)") {
		t.Fatalf("first event = %+v", events[0])
	}
	if events[1].DedupKey != "canary_interrupted:canary-2" ||
		!strings.Contains(events[1].Body, "No database was being changed") {
		t.Fatalf("second event = %+v", events[1])
	}
}

// Nil/empty: no interrupted run, no notification.
func TestCanaryInterruptedEventsNone(t *testing.T) {
	if got := canaryInterruptedEvents(nil); len(got) != 0 {
		t.Fatalf("events for no runs = %+v", got)
	}
}
