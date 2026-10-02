package sre

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// CHECK-04: when a pooler is configured, a connection investigation reads
// the pooler's telemetry at both samples (queueing must persist to count)
// and no other family pays for it. Without a pooler the plans are
// unchanged.

func callIDs(st planStep) map[probes.ID]int {
	out := map[probes.ID]int{}
	for _, c := range st.calls {
		out[c.id]++
	}
	return out
}

func TestPlans_PoolerJoinsBothConnectionSamples(t *testing.T) {
	c := &Coordinator{cfg: DefaultCoordinatorConfig("plan-pooler"),
		signals: []probes.ID{probes.ChangeFeed, probes.PoolerPools}}
	plan, ok := c.plan(TriggerConnections)
	if !ok || len(plan) != 2 {
		t.Fatalf("connection plan = %+v (%v)", plan, ok)
	}
	for i, st := range plan {
		if callIDs(st)[probes.PoolerPools] != 1 {
			t.Errorf("connection step %d = %v, want pooler_pools once", i+1, st.calls)
		}
	}
	if callIDs(plan[0])[probes.ChangeFeed] != 1 || callIDs(plan[1])[probes.ChangeFeed] != 0 {
		t.Errorf("the change feed belongs to the first step only: %+v", plan)
	}
	for kind := range triggerKinds {
		if kind == TriggerConnections {
			continue
		}
		p, ok := c.plan(kind)
		if !ok {
			continue
		}
		for i, st := range p {
			if callIDs(st)[probes.PoolerPools] != 0 {
				t.Errorf("%s step %d reads the pooler: %v", kind, i+1, st.calls)
			}
		}
	}
}

func TestPlans_NoPoolerNoPoolerCalls(t *testing.T) {
	c := &Coordinator{cfg: DefaultCoordinatorConfig("plan-pooler"),
		signals: []probes.ID{probes.ChangeFeed, probes.SLOStatus}}
	plan, _ := c.plan(TriggerConnections)
	for i, st := range plan {
		if callIDs(st)[probes.PoolerPools] != 0 {
			t.Fatalf("step %d reads a pooler that is not configured: %v", i+1, st.calls)
		}
	}
	if !seriesProbes[probes.PoolerPools] {
		t.Fatal("pooler telemetry is compared across samples: it must be a series probe")
	}
}

// The pooler probe is a signal: wired like the change feed, validated,
// and answered from its source, never from the SQL catalog.
func TestSignals_PoolerIsAValidSignal(t *testing.T) {
	served := false
	sig := SignalProbe{ID: probes.PoolerPools, Run: func(context.Context,
		probes.Args) probes.Result {
		served = true
		return probes.Result{ProbeID: probes.PoolerPools, Status: probes.StatusEmpty}
	}}
	if err := validateSignals([]SignalProbe{sig}); err != nil {
		t.Fatalf("pooler signal refused: %v", err)
	}
	r := withSignals(failingRunner{}, []SignalProbe{sig})
	if res := r.Run(context.Background(), probes.PoolerPools, probes.Args{}); !served ||
		res.Status != probes.StatusEmpty {
		t.Fatalf("pooler probe = %+v (served %v)", res, served)
	}
}

type failingRunner struct{}

func (failingRunner) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	return probes.Result{ProbeID: id, Status: probes.StatusError, Reason: "catalog"}
}
