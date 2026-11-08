package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/lakebaseobs"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Resolve the active history binding on every read, including after a config
// revoke/rebind. No registry lock is held while acquiring a database connection.
type lakebaseSREHistory struct{ rt *databaseRuntime }

func (rt *databaseRuntime) sreHistorySource() probes.HistoricalSource {
	if rt.provider != "lakebase" {
		return nil
	}
	return lakebaseSREHistory{rt}
}

func (h lakebaseSREHistory) ReadHistory(ctx context.Context, id probes.ID, args probes.Args) probes.Result {
	unavailable := func(reason string) probes.Result {
		return probes.Result{ProbeID: id, Version: "v1_history", ObservedAt: time.Now().UTC(),
			Status: probes.StatusUnsupported, Reason: reason}
	}
	rt := h.rt
	if rt == nil || rt.cfg == nil || rt.spec.Pool == nil || !rt.cfg.LakebaseObservability.Enabled {
		return unavailable("history_not_enabled")
	}
	if denied := probes.CheckReadPolicy(ctx, rt.sreReadPolicy(), id, args); denied != nil {
		return *denied
	}
	conn := rt.spec.Pool.Config().ConnConfig
	binding, ok := rt.cfg.LakebaseObservability.Binding(conn.Host, conn.Database)
	if !ok {
		return unavailable("history_binding_unavailable")
	}
	lakebaseTelemetryRegistry.Lock()
	st := lakebaseTelemetryRegistry.entries[rt.spec.Name]
	lakebaseTelemetryRegistry.Unlock()
	if st == nil {
		return unavailable("history_binding_unavailable")
	}
	st.mu.Lock()
	runtime, store, payload := st.runtime, st.store, st.payload
	st.mu.Unlock()
	if runtime == nil || store == nil || payload != rt.cfg.LakebaseObservability.AllowPayload {
		return unavailable("history_binding_unavailable")
	}
	verified, _ := rt.lakebaseScope.Load().(lakebaseVerifiedBinding)
	scope := runtime.Scope()
	policy := lakebasePolicyBinding(binding)
	if verified.ScopeHash == "" || verified.ScopeHash != scope.Hash() ||
		verified.PolicyBinding != policy || scope.PolicyBinding != policy || scope.DatabaseName != conn.Database {
		return unavailable("history_binding_changed")
	}
	source, err := lakebaseobs.NewEvidenceSource(store, runtime, rt.spec.Pool)
	if err != nil {
		return unavailable("history_binding_unavailable")
	}
	return source.ReadHistory(probes.WithReadPolicy(ctx, rt.sreReadPolicy()), id, args)
}
