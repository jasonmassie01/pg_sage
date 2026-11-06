package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Review of facts-02: the runtime wiring itself. A runtime joins the
// process's cluster registry when its facts start, its gate binder reads
// the siblings' slot facts, and stopping a runtime leaves the cluster.

// factRuntime is a database runtime with only its facts started, the way
// newDatabaseRuntime starts them.
func factRuntime(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	name string) *databaseRuntime {
	t.Helper()
	rt := &databaseRuntime{spec: databaseRuntimeSpec{Name: name, Pool: pool}}
	rt.ctx, rt.cancel = context.WithCancel(ctx)
	t.Cleanup(rt.cancel)
	rt.initFacts()
	if rt.facts == nil || rt.factCluster == nil || rt.factCluster.id.sysid == "" {
		t.Fatalf("%s: facts not started: store=%v member=%+v", name, rt.facts,
			rt.factCluster)
	}
	return rt
}

// isolateProcessFactClusters gives the test its own process registry.
func isolateProcessFactClusters(t *testing.T) {
	t.Helper()
	old := processFactClusters
	processFactClusters = newFactClusterRegistry()
	t.Cleanup(func() { processFactClusters = old })
}

func TestFactClusterRuntimeWiringOnRealCluster(t *testing.T) {
	c := newClusterPair(t)
	fact := confirmSlotFact(t, c)
	isolateProcessFactClusters(t)
	rtA := factRuntime(t, c.ctx, c.poolA, "facts_a")
	rtB := factRuntime(t, c.ctx, c.poolB, "facts_b")
	if n := processFactClusters.size(); n != 2 {
		t.Fatalf("process registry holds %d runtimes, want 2", n)
	}
	bound := walBoundOfB(t, c)
	req := policy.ActionRequest{SQL: bound.SQL, TargetObjs: bound.TargetObjects}
	got, err := rtB.factBinder().Bind(c.ctx, req)
	if err != nil || len(got) != 1 || got[0].FactID != fact.ID ||
		got[0].Database != "facts_a" || got[0].Route != "alert" {
		t.Fatalf("B's runtime binder on %q: %+v %v, want A's fact %d", bound.SQL, got, err,
			fact.ID)
	}
	// A's own runtime binder binds its own fact, not as a sibling's.
	got, err = rtA.factBinder().Bind(c.ctx, req)
	if err != nil || len(got) != 1 || got[0].FactID != fact.ID || got[0].Database != "" {
		t.Fatalf("A's runtime binder: %+v %v, want its own fact %d", got, err, fact.ID)
	}
	rtA.cancel()
	deadline := time.Now().Add(5 * time.Second)
	for processFactClusters.size() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("a stopped runtime stayed in the process registry")
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err = rtB.factBinder().Bind(c.ctx, req)
	if err != nil || len(got) != 0 {
		t.Fatalf("B still bound by a stopped sibling: %+v %v", got, err)
	}
}
