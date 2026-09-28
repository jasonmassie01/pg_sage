package main

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// parityModeBuilder builds one mode's runtime for the fixture database and
// returns its fleet registration.
type parityModeBuilder func(t *testing.T, base *config.Config, dsn string) *fleet.DatabaseInstance

// TestRuntimeParityEquivalentDatabase builds the same database through every
// deployment mode and requires identical safety wiring: the standing gate,
// the cloud environment, the managed-config adapter, the trust level, the
// database name (never "all") and lifecycle ownership (G5-I07).
func TestRuntimeParityEquivalentDatabase(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	markFixtureAsAzure(t, dsn)
	stubAzureToken(t, nil)
	base := parityBaseConfig(t, dsn)
	modes := map[string]parityModeBuilder{
		"standalone": buildStandaloneParity, "yaml-fleet": buildFleetParity,
		"meta-db": buildMetaParity, "agentdb": buildAgentDBParity,
	}
	names := make([]string, 0, len(modes))
	for mode := range modes {
		names = append(names, mode)
	}
	sort.Strings(names)
	for _, mode := range names {
		t.Run(mode, func(t *testing.T) {
			inst := modes[mode](t, base, dsn)
			if inst == nil {
				t.Fatalf("%s registered no runtime", mode)
			}
			assertParityProbe(t, mode, inst)
		})
	}
}

func assertParityProbe(t *testing.T, mode string, inst *fleet.DatabaseInstance) {
	t.Helper()
	if inst.Name == "" || inst.Name == "all" {
		t.Fatalf("%s runtime name %q is not a database name", mode, inst.Name)
	}
	want := map[string]string{
		"collector": "true", "analyzer": "true", "executor": "true",
		"cancel": "true", "workers": "true",
		"gate": "true", "provider": "azure", "managed_config": "true",
		"trust": "advisory", "exec.database_name": inst.Name,
		"execution_mode": "auto", "executor_enabled": "true",
		"dispatcher": "true", "action_store": "true",
		"analyze_semaphore": "true", "post_ddl_hook": "true",
		"status.database_name": inst.Name, "status.platform": "azure",
		"status.trust": "advisory", "status.pg_version": "known",
		"status.connected": "true",
	}
	got := parityProbe(inst)
	keys := make([]string, 0, len(want))
	for key := range want {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if got[key] != want[key] {
			t.Errorf("%s: %s = %q, want %q", mode, key, got[key], want[key])
		}
	}
}

func parityProbe(inst *fleet.DatabaseInstance) map[string]string {
	settings := inst.Executor.RuntimeSettings()
	status := inst.SnapshotStatus()
	version := "unknown"
	if status.PGVersion != "" && status.PGVersion != "unknown" {
		version = "known"
	}
	return map[string]string{
		"collector": fmt.Sprint(inst.Collector != nil),
		"analyzer":  fmt.Sprint(inst.Analyzer != nil),
		"executor":  fmt.Sprint(inst.Executor != nil),
		"cancel":    fmt.Sprint(inst.Cancel != nil),
		"workers":   fmt.Sprint(inst.Workers != nil),
		"gate":      fmt.Sprint(settings.PolicyGate), "provider": settings.Provider,
		"managed_config":       fmt.Sprint(settings.ManagedConfig),
		"trust":                settings.TrustLevel,
		"exec.database_name":   settings.DatabaseName,
		"execution_mode":       settings.ExecutionMode,
		"executor_enabled":     fmt.Sprint(settings.ExecutorEnabled),
		"dispatcher":           fmt.Sprint(settings.Dispatcher),
		"action_store":         fmt.Sprint(settings.ActionStore),
		"analyze_semaphore":    fmt.Sprint(settings.AnalyzeSemaphore),
		"post_ddl_hook":        fmt.Sprint(settings.PostDDLHook),
		"status.database_name": status.DatabaseName,
		"status.platform":      status.Platform, "status.trust": status.TrustLevel,
		"status.pg_version": version, "status.connected": fmt.Sprint(status.Connected),
	}
}

// markFixtureAsAzure makes cloud detection report azure for new connections:
// detectCloudEnv reads the azure.extensions setting.
func markFixtureAsAzure(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect fixture: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	database := pgx.Identifier{conn.Config().Database}.Sanitize()
	if _, err := conn.Exec(ctx, "ALTER DATABASE "+database+
		" SET azure.extensions = 'pg_stat_statements'"); err != nil {
		t.Fatalf("mark fixture as azure: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Errorf("reconnect fixture: %v", err)
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		if _, err := c.Exec(context.Background(), "ALTER DATABASE "+database+
			" RESET azure.extensions"); err != nil {
			t.Errorf("reset azure marker: %v", err)
		}
	})
}

func parityBaseConfig(t *testing.T, dsn string) *config.Config {
	t.Helper()
	conn, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	base := config.DefaultConfig()
	base.Trust.Level = "advisory"
	base.Collector.IntervalSeconds, base.Analyzer.IntervalSeconds = 3600, 3600
	base.LLM.Enabled = false
	base.Azure.SubscriptionID, base.Azure.ResourceGroup = "sub-parity", "rg-parity"
	base.Azure.ServerName = "parity-server"
	base.Postgres = config.PostgresConfig{
		Host: conn.Host, Port: int(conn.Port), User: conn.User,
		Password: conn.Password, Database: conn.Database,
		SSLMode: "disable", MaxConnections: 6,
	}
	return base
}

func parityDatabaseConfig(base *config.Config, name string) config.DatabaseConfig {
	return config.DatabaseConfig{
		Name: name, Host: base.Postgres.Host, Port: base.Postgres.Port,
		User: base.Postgres.User, Password: base.Postgres.Password,
		Database: base.Postgres.Database, SSLMode: "disable", MaxConnections: 6,
	}
}

// preserveParityGlobals snapshots every process global a mode builder
// writes, gives the builder a cancellable process context, and drains the
// runtimes it registered.
func preserveParityGlobals(t *testing.T, base *config.Config) {
	t.Helper()
	preserveFleetRuntimeGlobals(t)
	oldPool, oldCtx, oldCancel := pool, shutdownCtx, shutdownCancel
	oldMeta, oldCloud, oldRamp := globalMetaState, cloudEnvironment, configRampStart
	oldColl, oldAnal, oldExec, oldActions := coll, anal, exec, actionStore
	ctx, cancel := context.WithCancel(context.Background())
	shutdownCtx, shutdownCancel = ctx, cancel
	cfg = config.Clone(base)
	llmClient, llmMgr, configController, fleetLLMBudget = nil, nil, nil, nil
	globalMetaState, configRampStart = nil, time.Time{}
	fleetMgr = fleet.NewManager(cfg)
	t.Cleanup(func() {
		drainParityRuntimes(t)
		cancel()
		pool, shutdownCtx, shutdownCancel = oldPool, oldCtx, oldCancel
		globalMetaState, cloudEnvironment, configRampStart = oldMeta, oldCloud, oldRamp
		coll, anal, exec, actionStore = oldColl, oldAnal, oldExec, oldActions
	})
}

func drainParityRuntimes(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for name, inst := range fleetMgr.Instances() {
		if inst.Cancel == nil {
			continue
		}
		if err := fleet.ShutdownInstance(ctx, inst); err != nil {
			t.Errorf("drain %s runtime: %v", name, err)
		}
	}
}

func buildStandaloneParity(
	t *testing.T, base *config.Config, dsn string,
) *fleet.DatabaseInstance {
	preserveParityGlobals(t, base)
	cfg.Mode = "standalone"
	monitored, err := connectMonitoredDB(dsn, cfg.Postgres.MaxConnections)
	if err != nil {
		t.Fatalf("connect standalone pool: %v", err)
	}
	t.Cleanup(monitored.Close)
	pool = monitored
	cloudEnvironment = detectCloudEnvironment()
	cfg.CloudEnvironment = cloudEnvironment
	initStandalone()
	return fleetMgr.GetInstance(resolveDBName())
}

func buildFleetParity(
	t *testing.T, base *config.Config, _ string,
) *fleet.DatabaseInstance {
	preserveParityGlobals(t, base)
	cfg.Mode = "fleet"
	name := base.Postgres.Database
	cfg.Databases = []config.DatabaseConfig{parityDatabaseConfig(base, name)}
	initFleetMultiDB()
	return fleetMgr.GetInstance(name)
}

func buildMetaParity(
	t *testing.T, base *config.Config, dsn string,
) *fleet.DatabaseInstance {
	preserveParityGlobals(t, base)
	cfg.Mode = "fleet"
	control, err := connectMetaDB(dsn)
	if err != nil {
		t.Fatalf("connect meta pool: %v", err)
	}
	t.Cleanup(control.Close)
	globalMetaState = &metaDBState{Pool: control}
	dbCfg := parityDatabaseConfig(base, base.Postgres.Database)
	rec := store.DatabaseRecord{
		ID: 77, Name: dbCfg.Name, Host: dbCfg.Host, Port: dbCfg.Port,
		DatabaseName: dbCfg.Database, Username: dbCfg.User, SSLMode: "disable",
		MaxConnections: dbCfg.MaxConnections, TrustLevel: base.Trust.Level,
		ExecutionMode: "auto",
	}
	inst, err := prepareStoreDatabaseConnection(context.Background(), rec, dsn)
	if err != nil {
		t.Fatalf("build meta runtime: %v", err)
	}
	activateStoreDatabaseWithManager(fleetMgr, inst)
	return inst
}

func buildAgentDBParity(
	t *testing.T, base *config.Config, dsn string,
) *fleet.DatabaseInstance {
	preserveParityGlobals(t, base)
	cfg.Mode = "fleet"
	control, err := connectMetaDB(dsn)
	if err != nil {
		t.Fatalf("connect control pool: %v", err)
	}
	t.Cleanup(control.Close)
	// The reconciler only runs with an auth pool: the primary instance.
	fleetMgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "primary", Pool: control, Status: &fleet.InstanceStatus{},
	})
	dbCfg := parityDatabaseConfig(base, agentFleetPrefix+"parity")
	connectAgentDBToFleet(context.Background(), fleetMgr, dbCfg)
	return fleetMgr.GetInstance(dbCfg.Name)
}
