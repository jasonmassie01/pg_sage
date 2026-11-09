package main

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent access tests: each runtime here is private to its test and
// the global cfg is swapped only for the test's own duration.

// The runtime passes its own databases[].cloud_resource to telemetry: a
// fleet database behind the Auth Proxy on loopback polls its named instance.
func TestRuntimeCloudTelemetryReadsTheDatabaseResource(t *testing.T) {
	rt := &databaseRuntime{cfg: cloudCfg(), provider: "cloud-sql",
		spec: databaseRuntimeSpec{Name: "orders", Scope: "fleet",
			Pool: lazyPool(t, "orders"),
			Config: config.DatabaseConfig{Name: "orders",
				CloudResource: gcpResource("proj-1", "main")}}}
	got, _ := runtimeCloudTelemetry(context.Background(), rt, testCloudDeps())
	assertPolling(t, got, "cloud-sql")
}

// Without a resource the same runtime is refused before any credential call.
func TestRuntimeCloudTelemetryWithoutResourceNamesTheAuthProxy(t *testing.T) {
	calls := 0
	rt := &databaseRuntime{cfg: cloudCfg(), provider: "cloud-sql",
		spec: databaseRuntimeSpec{Name: "orders", Scope: "fleet",
			Pool:   lazyPool(t, "orders"),
			Config: config.DatabaseConfig{Name: "orders"}}}
	got, _ := runtimeCloudTelemetry(context.Background(), rt, countingDeps(&calls))
	if got == nil {
		t.Fatal("runtime = nil")
	}
	if st := got.Status(); st.Available || !strings.Contains(st.Reason, "Auth Proxy") {
		t.Fatalf("status = %+v, want unavailable naming the Auth Proxy", st)
	}
	if calls != 0 {
		t.Fatalf("credential calls = %d, want 0", calls)
	}
}

func swapGlobalConfig(t *testing.T, c *config.Config) {
	t.Helper()
	old := cfg
	cfg = c
	t.Cleanup(func() { cfg = old })
}

// Standalone: the runtime's database carries postgres.provider and
// postgres.cloud_resource, whichever way it is built.
func TestBuildDBConfigCarriesThePostgresIdentity(t *testing.T) {
	resource := config.CloudResourceConfig{AWS: config.CloudResourceAWS{
		DBInstanceIdentifier: "orders-prod", Region: "eu-west-1"}}
	c := config.DefaultConfig()
	c.Postgres.Host, c.Postgres.Database = "10.0.0.5", "orders"
	c.Postgres.Provider, c.Postgres.CloudResource = "rds", resource
	c.Databases = nil
	swapGlobalConfig(t, c)
	got := buildDBConfig("orders")
	if got.Provider != "rds" || got.CloudResource != resource || got.Host != "10.0.0.5" {
		t.Fatalf("database = %+v, want provider rds and the postgres resource", got)
	}
}
