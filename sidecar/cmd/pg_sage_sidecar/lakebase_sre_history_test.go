package main

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/lakebaseobs"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

func TestLakebaseSREHistoryDynamicBindingAndRevocation(t *testing.T) {
	f := newHistoryWiringFixture(t)
	rt, scope, _ := f.registration(t)
	runtime, _ := f.runtime(t, scope)
	event := historyWiringEvent(t, scope, lakebaseobs.Logs, f.now.Add(-time.Minute), f.now, nil)
	f.insert(t, scope, runtime, event)
	if err := f.store.ActivateOwner(f.ctx, scope, true); err != nil {
		t.Fatal(err)
	}
	rt.lakebaseScope.Store(lakebaseVerifiedBinding{ScopeHash: scope.Hash(), PolicyBinding: scope.PolicyBinding})
	source := rt.sreHistorySource()
	if r := source.ReadHistory(f.ctx, probes.LakebaseLogs, probes.Args{}); r.Reason != "history_binding_unavailable" {
		t.Fatalf("missing registry=%+v", r)
	}
	state := &lakebaseTelemetryState{runtime: runtime, store: f.store, payload: true}
	registerHistoryWiring(t, rt.spec.Name, state)
	r := source.ReadHistory(f.ctx, probes.LakebaseLogs, probes.Args{})
	if r.Status != probes.StatusOK || len(r.Rows) != 1 || r.History == nil || r.History.Complete {
		t.Fatalf("bound history=%+v", r)
	}
	// A retained wrapper must re-read both config and registry each time.
	rt.cfg.LakebaseObservability.Enabled = false
	if r = source.ReadHistory(f.ctx, probes.LakebaseLogs, probes.Args{}); r.Reason != "history_not_enabled" {
		t.Fatalf("disabled=%+v", r)
	}
	rt.cfg.LakebaseObservability.Enabled = true
	rt.cfg.LakebaseObservability.Bindings[0].Endpoint += "-changed"
	if r = source.ReadHistory(f.ctx, probes.LakebaseLogs, probes.Args{}); r.Reason != "history_binding_changed" {
		t.Fatalf("rebound=%+v", r)
	}
	rt.cfg.LakebaseObservability.Bindings[0].Endpoint = "projects/owned-test/branches/owned-test/endpoints/primary"
	rt.cfg.SRE.ReadPolicy.AllowedSources = []string{string(probes.StatTables)}
	if r = source.ReadHistory(f.ctx, probes.LakebaseLogs, probes.Args{}); r.Reason != probes.ReasonPolicyDenied {
		t.Fatalf("narrowed=%+v", r)
	}
	rt.cfg.SRE.ReadPolicy.AllowedSources = nil
	state.mu.Lock()
	state.runtime = nil
	state.mu.Unlock()
	if r = source.ReadHistory(f.ctx, probes.LakebaseLogs, probes.Args{}); r.Reason != "history_binding_unavailable" {
		t.Fatalf("retired runtime=%+v", r)
	}
}
