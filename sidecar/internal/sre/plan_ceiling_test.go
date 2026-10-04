package sre

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Sage SRE M6 integration: M5 adds the signal probes to the first step
// of every plan, and the M6 reactive and runway families bring their own
// plans. Every trigger kind with a plan must collect both signals and
// still leave the model turn's one probe under the probe ceiling.
func TestPlans_WithBothSignalsLeaveRoomForTheModelProbe(t *testing.T) {
	c := &Coordinator{cfg: DefaultCoordinatorConfig("plan-ceiling"),
		signals: []probes.ID{probes.ChangeFeed, probes.SLOStatus}}
	planned := 0
	for kind := range triggerKinds {
		plan, ok := c.plan(kind)
		if !ok {
			t.Errorf("%s has no probe plan", kind)
			continue
		}
		planned++
		total := 0
		for _, st := range plan {
			total += len(st.calls)
		}
		if total > CeilingProbes-1 {
			t.Errorf("%s plans %d probes with both signals, want at most %d", kind,
				total, CeilingProbes-1)
		}
		first := map[probes.ID]bool{}
		for _, call := range plan[0].calls {
			first[call.id] = true
		}
		if !first[probes.ChangeFeed] || !first[probes.SLOStatus] {
			t.Errorf("%s first step lacks a signal probe: %v", kind, plan[0].calls)
		}
	}
	// lock, connections, WAL, plan, SLO burn, operator triage (roadmap
	// 2.1), 4 reactive, 3 runway kinds.
	if planned != 13 {
		t.Fatalf("%d trigger kinds have a plan, want 13", planned)
	}
}

// Without wired signals a plan is the family's own plan, unchanged.
func TestPlans_WithoutSignalsAreTheFamilyPlan(t *testing.T) {
	c := &Coordinator{cfg: DefaultCoordinatorConfig("plan-ceiling")}
	for _, kind := range []TriggerKind{TriggerLock, TriggerDiskWAL, TriggerLWLock} {
		plan, ok := c.plan(kind)
		if !ok {
			t.Fatalf("%s has no plan", kind)
		}
		for _, call := range plan[0].calls {
			if probes.IsSignal(call.id) {
				t.Errorf("%s plan has signal probe %s without signals wired", kind, call.id)
			}
		}
	}
}
