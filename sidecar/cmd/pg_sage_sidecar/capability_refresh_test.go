package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// The runtime's refresher is an instance worker: a reload request reaches
// it through the fleet manager, and cancelling the runtime drains it.
func TestCapabilityRefreshWorkerServesReloadAndDrains(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	inst := &fleet.DatabaseInstance{Name: "db", Status: &fleet.InstanceStatus{
		Capabilities: fleet.ProviderCapabilities{Provider: "postgres", ReadyForAutoSafe: true,
			Permissions: map[string]fleet.CapabilityStatus{"analyze": {Status: "ok"}}}}}
	rt := &databaseRuntime{ctx: ctx, cancel: cancel, workers: &sync.WaitGroup{},
		provider: "postgres", inst: inst}
	rt.startCapabilityRefresh()
	if inst.CapabilityRefresh == nil {
		t.Fatal("runtime registered no capability refresh hook")
	}
	saved := fleetMgr
	t.Cleanup(func() { fleetMgr = saved })
	fleetMgr = fleet.NewManager(&config.Config{Mode: "fleet"})
	fleetMgr.RegisterInstance(inst)
	requestCapabilityRefresh()
	deadline := time.Now().Add(5 * time.Second)
	for inst.SnapshotStatus().Capabilities.Permissions["analyze"].Status != "unknown" {
		if time.Now().After(deadline) {
			t.Fatal("reload request did not refresh the capabilities")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if inst.SnapshotStatus().Capabilities.ReadyForAutoSafe {
		t.Fatal("refresh without a pool kept auto-safe readiness")
	}
	cancel()
	drained := make(chan struct{})
	go func() { rt.workers.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("capability refresh worker did not stop with its runtime")
	}
}

func TestRequestCapabilityRefreshWithoutManager(t *testing.T) {
	saved := fleetMgr
	t.Cleanup(func() { fleetMgr = saved })
	fleetMgr = nil
	done := make(chan struct{})
	go func() {
		defer close(done)
		requestCapabilityRefresh() // standalone start-up before the manager exists
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("requestCapabilityRefresh blocked without a fleet manager")
	}
	if fleetMgr != nil {
		t.Fatal("requestCapabilityRefresh created a fleet manager")
	}
}

func TestCapabilityRefreshInterval(t *testing.T) {
	if capabilityRefreshInterval != 15*time.Minute {
		t.Fatalf("capability refresh interval = %s, want 15m", capabilityRefreshInterval)
	}
}
