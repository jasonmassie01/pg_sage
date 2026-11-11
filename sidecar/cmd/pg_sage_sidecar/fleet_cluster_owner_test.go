package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// forecast-09 wiring: a fleet runtime (one in the instance registry)
// shares the process's runway owners and joins its instance's forecast
// owners by system identifier; single-database mode and an address-only
// key keep every runway and forecast per database.

func TestFleetClusterOwner_RunwayOwnersOnlyInFleetMode(t *testing.T) {
	cfg := config.DefaultConfig()
	single := &databaseRuntime{cfg: cfg, spec: databaseRuntimeSpec{Name: "orders"}}
	if single.runwayMonitorOptions().Owners != nil {
		t.Fatal("a runtime outside the instance registry got the shared owners")
	}
	fleetRT := &databaseRuntime{cfg: cfg, spec: databaseRuntimeSpec{Name: "orders"},
		instanceOwner: &instanceMember{database: "orders"}}
	if fleetRT.runwayMonitorOptions().Owners != runwayClusterOwners {
		t.Fatal("a fleet runtime did not get the process's runway owners")
	}
}

func TestFleetClusterOwner_ForecastOwnerNeedsASystemIdentifier(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if (&databaseRuntime{ctx: ctx}).forecastClusterOwner() != nil {
		t.Fatal("single-database mode got a forecast owner")
	}
	addr := &databaseRuntime{ctx: ctx, spec: databaseRuntimeSpec{Name: "orders"},
		instanceOwner: &instanceMember{key: instanceKey{key: "db:5432",
			source: instanceKeyAddress}}}
	if addr.forecastClusterOwner() != nil {
		t.Fatal("an address key grouped forecasts; a proxy may front several clusters")
	}
	key := instanceKey{key: "sysid:forecast09-wiring", source: instanceKeySystemID}
	bCtx, stopB := context.WithCancel(ctx)
	cfg := config.DefaultConfig()
	a := (&databaseRuntime{ctx: ctx, cfg: cfg, spec: databaseRuntimeSpec{Name: "alpha"},
		instanceOwner: &instanceMember{key: key}}).forecastClusterOwner()
	b := (&databaseRuntime{ctx: bCtx, cfg: cfg, spec: databaseRuntimeSpec{Name: "beta"},
		instanceOwner: &instanceMember{key: key}}).forecastClusterOwner()
	if a == nil || b == nil {
		t.Fatal("a system-identifier key gave no forecast owner")
	}
	b.IsClusterOwner()
	if owner, ok := a.IsClusterOwner(); owner != "alpha" || !ok {
		t.Fatalf("owner = %q %v, want alpha", owner, ok)
	}
	if owner, ok := b.IsClusterOwner(); owner != "alpha" || ok {
		t.Fatalf("beta sees %q %v, want alpha owning", owner, ok)
	}
	stopB()
	deadline := time.Now().Add(5 * time.Second)
	for slices.Contains(forecastClusterOwners.Members(key.key), "beta") {
		if time.Now().After(deadline) {
			t.Fatal("beta's membership outlived its runtime")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := a.Databases(); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("members after beta ended = %v", got)
	}
}

// No concurrency test here: the election is raced in internal/runway.
