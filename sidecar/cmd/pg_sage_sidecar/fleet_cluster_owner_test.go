package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
	if (&databaseRuntime{ctx: ctx, spec: databaseRuntimeSpec{Name: "orders"},
		instanceOwner: &instanceMember{key: key}}).forecastClusterOwner() != nil {
		t.Fatal("a runtime without a pool (no address) got a forecast owner")
	}
}

// forecastRuntimeAt is a fleet runtime of key reached at host.
func forecastRuntimeAt(t *testing.T, ctx context.Context, key instanceKey, name,
	host string) *databaseRuntime {
	t.Helper()
	return &databaseRuntime{ctx: ctx, cfg: config.DefaultConfig(),
		instanceOwner: &instanceMember{key: key},
		spec:          databaseRuntimeSpec{Name: name, Pool: addressPool(t, host, name)}}
}

// One cluster's runtimes share a forecast owner; a restored clone (or a
// promoted standby) shares the system identifier at another address and
// owns its own forecasts; a stopped runtime leaves.
func TestFleetClusterOwner_ForecastOwnerPerClusterAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := instanceKey{key: "sysid:forecast09-wiring", source: instanceKeySystemID}
	bCtx, stopB := context.WithCancel(ctx)
	aRT := forecastRuntimeAt(t, ctx, key, "alpha", "db-a")
	a := aRT.forecastClusterOwner()
	b := forecastRuntimeAt(t, bCtx, key, "beta", "DB-A").forecastClusterOwner()
	clone := forecastRuntimeAt(t, ctx, key, "beta_clone", "db-b").forecastClusterOwner()
	if a == nil || b == nil || clone == nil {
		t.Fatal("a system-identifier key gave no forecast owner")
	}
	b.IsClusterOwner()
	if owner, ok := a.IsClusterOwner(); owner != "alpha" || !ok {
		t.Fatalf("owner = %q %v, want alpha", owner, ok)
	}
	if owner, ok := b.IsClusterOwner(); owner != "alpha" || ok {
		t.Fatalf("beta sees %q %v, want alpha owning", owner, ok)
	}
	if owner, ok := clone.IsClusterOwner(); owner != "beta_clone" || !ok {
		t.Fatalf("the clone sees %q %v, want it owning its own forecasts", owner, ok)
	}
	full := forecastOwnerKey(key, aRT.spec.Pool)
	if got := forecastClusterOwners.Members(full); !slices.Equal(got,
		[]string{"alpha", "beta"}) {
		t.Fatalf("members of %s = %v, want alpha and beta", full, got)
	}
	stopB()
	deadline := time.Now().Add(5 * time.Second)
	for slices.Contains(forecastClusterOwners.Members(full), "beta") {
		if time.Now().After(deadline) {
			t.Fatal("beta's membership outlived its runtime")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := a.Databases(); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("members after beta ended = %v", got)
	}
}

// addressPool is a pool that never connects (no minimum connections):
// the forecast owner key only reads its address.
func addressPool(t *testing.T, host, database string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://sage@"+host+":5432/"+database+"?sslmode=disable")
	if err != nil {
		t.Fatalf("pool for %s: %v", host, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// No concurrency test here: the election is raced in internal/runway.
