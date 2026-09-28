package main

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestAgentCollectorSurvivesReconcileContextAndDrainsWithOwner(t *testing.T) {
	state, input, _ := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connectAgentDBToFleet(ctx, fleetMgr, config.DatabaseConfig{
		Name: "agentdb:fixture", Host: input.Host, Port: input.Port,
		User: input.Username, Password: input.Password, Database: input.DatabaseName,
		SSLMode: "disable", MaxConnections: 3,
	})
	inst := fleetMgr.GetInstance("agentdb:fixture")
	if inst == nil || inst.Collector == nil || inst.Cancel == nil || inst.Workers == nil ||
		inst.Config.Database != state.Pool.Config().ConnConfig.Database {
		t.Fatal("agent collector ownership incorrect")
	}
	// Agent databases get the same runtime as every other mode: any
	// authority they have comes only from the standing policy gate.
	if settings := inst.Executor.RuntimeSettings(); !settings.PolicyGate ||
		settings.DatabaseName != "agentdb:fixture" {
		t.Fatalf("agent executor is not governed by the standing gate: %+v", settings)
	}
	cancel()
	if agentDBRuntimeParent(ctx).Err() != nil || inst.Pool.Ping(context.Background()) != nil {
		t.Fatal("reconcile timeout canceled long-lived agent runtime")
	}
	shutdownCtx = nil
	if agentDBRuntimeParent(ctx).Err() != nil {
		t.Fatal("missing process context inherited expired reconcile context")
	}
}
