package main

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: the fleet globals are swapped for one
// sequential build at a time (preserveParityGlobals).

// Integration: a YAML fleet database's cloud_resource.azure reaches its
// executor's ARM adapter when the process azure.* settings name nothing;
// without it the same database stays guidance-only.
func TestFleetAzureResourceInstallsTheManagedConfigAdapter(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	markFixtureAsAzure(t, dsn)
	stubAzureToken(t, nil)
	resource := config.CloudResourceAzure{
		Subscription:  "0b1f6471-1bf0-4dda-aec3-111122223333",
		ResourceGroup: "rg-orders", Server: "orders-flex"}
	for _, tc := range []struct {
		name     string
		resource config.CloudResourceAzure
		want     bool
	}{
		{"with cloud_resource.azure", resource, true},
		{"without a resource", config.CloudResourceAzure{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildFleetAzureRuntime(t, dsn, tc.resource)
			if got.Provider != "azure" || got.ManagedConfig != tc.want {
				t.Fatalf("executor provider=%s managed_config=%t, want azure and %t",
					got.Provider, got.ManagedConfig, tc.want)
			}
		})
	}
}

// fleetAzureSettings is what the test reads off the built executor.
type fleetAzureSettings struct {
	Provider      string
	ManagedConfig bool
}

func buildFleetAzureRuntime(t *testing.T, dsn string,
	resource config.CloudResourceAzure) fleetAzureSettings {
	t.Helper()
	base := parityBaseConfig(t, dsn)
	base.Azure = config.AzureConfig{}
	captureParityRuntimes(t)
	preserveParityGlobals(t, base)
	cfg.Mode = "fleet"
	name := base.Postgres.Database
	db := parityDatabaseConfig(base, name)
	db.CloudResource.Azure = resource
	cfg.Databases = []config.DatabaseConfig{db}
	initFleetMultiDB()
	inst := fleetMgr.GetInstance(name)
	if inst == nil || inst.Pool == nil {
		t.Fatalf("fleet registered no runtime for %s", name)
	}
	control := inst.Pool
	t.Cleanup(func() {
		if _, err := control.Exec(context.Background(),
			"DELETE FROM sage.databases WHERE name = $1", name); err != nil {
			t.Errorf("remove the fleet registration of %s: %v", name, err)
		}
	})
	rt := parityRuntimes[name]
	if rt == nil || rt.executor == nil {
		t.Fatalf("runtime %s has no executor", name)
	}
	s := rt.executor.RuntimeSettings()
	return fleetAzureSettings{Provider: s.Provider, ManagedConfig: s.ManagedConfig}
}
