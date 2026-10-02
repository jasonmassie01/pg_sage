package main

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/schema"
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
	captureParityRuntimes(t)
	// D8: a stop persisted in the database must be restored when each
	// mode registers the runtime, not only when an operator acts again.
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprintf("persisted_stop=%t", stopped), func(t *testing.T) {
			persistParityStop(t, dsn, stopped)
			runParityModes(t, base, dsn, stopped)
		})
	}
}

func runParityModes(t *testing.T, base *config.Config, dsn string, stopped bool) {
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
			assertParityProbe(t, mode, inst, stopped)
		})
	}
}

// persistParityStop writes the durable emergency-stop flag in the fixture
// database (bootstrapping the schema first) and clears it afterwards.
func persistParityStop(t *testing.T, dsn string, stopped bool) {
	t.Helper()
	ctx := context.Background()
	p, err := connectMetaDB(dsn)
	if err != nil {
		t.Fatalf("connect fixture: %v", err)
	}
	t.Cleanup(p.Close)
	if err := schema.Bootstrap(ctx, p); err != nil {
		t.Fatalf("bootstrap fixture: %v", err)
	}
	if err := executor.SetEmergencyStop(ctx, p, stopped, "parity-test"); err != nil {
		t.Fatalf("persist stop: %v", err)
	}
	t.Cleanup(func() {
		if err := executor.SetEmergencyStop(context.Background(), p, false,
			"parity-test"); err != nil {
			t.Errorf("clear stop: %v", err)
		}
	})
}

func assertParityProbe(
	t *testing.T, mode string, inst *fleet.DatabaseInstance, stopped bool,
) {
	t.Helper()
	if inst.Name == "" || inst.Name == "all" {
		t.Fatalf("%s runtime name %q is not a database name", mode, inst.Name)
	}
	want := map[string]string{
		"io_admission": "true", "value_source": "own pool",
		"sre_fast_path": "instance workers", "sre_probes": "true",
		// Sage SRE M2: every mode runs the investigator on its own worker
		// group, bound to a database UUID, sharing the RCA probe runner,
		// and exposes it to the API through the fleet registration.
		"sre_investigator": "instance workers", "sre_scope": "bound",
		"sre_probe_runner": "shared", "sre_service": "registered",
		// Sage SRE M5: every mode registers the action service.
		"sre_actions": "registered",
		"stopped":     fmt.Sprint(stopped), "stop_attributed": fmt.Sprint(stopped),
		"collector": "true", "analyzer": "true", "executor": "true",
		"cancel": "true", "workers": "true",
		"gate": "true", "provider": "azure", "managed_config": "true",
		"trust": "advisory", "exec.database_name": inst.Name,
		"execution_mode": "auto", "executor_enabled": fmt.Sprint(!stopped),
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
	rt := parityRuntimes[inst.Name]
	return map[string]string{
		"sre_fast_path":    paritySREFastPath(rt, inst),
		"sre_probes":       fmt.Sprint(parityProbesAttached(rt)),
		"sre_investigator": paritySREInvestigator(rt, inst),
		"sre_scope":        paritySREScope(rt),
		"sre_probe_runner": paritySREProbeRunner(rt),
		"sre_service":      paritySREService(rt, inst),
		"sre_actions":      paritySREActions(inst),
		"io_admission":     fmt.Sprint(settings.IOEvidence),
		"value_source":     parityValueSource(inst),
		"stopped":          fmt.Sprint(fleetMgr.InstanceStopped(inst)),
		"stop_attributed": fmt.Sprint(inst.StoppedBy == "parity-test" &&
			!inst.StoppedAt.IsZero()),
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

// parityRuntimes records every runtime a parity build completes, by name.
var parityRuntimes = map[string]*databaseRuntime{}

func captureParityRuntimes(t *testing.T) {
	t.Helper()
	old := runtimeBuilt
	runtimeBuilt = func(rt *databaseRuntime) { parityRuntimes[rt.spec.Name] = rt }
	t.Cleanup(func() {
		runtimeBuilt = old
		parityRuntimes = map[string]*databaseRuntime{}
	})
}

// paritySREFastPath reports whether the SRE lock-chain fast path runs on the
// runtime's own worker group (the instance drains it on removal).
func paritySREFastPath(rt *databaseRuntime, inst *fleet.DatabaseInstance) string {
	switch {
	case rt == nil || rt.rcaAdapter == nil:
		return "no rca adapter"
	case rt.rcaAdapter.fastPath == nil:
		return "not started"
	case rt.workers != inst.Workers:
		return "foreign workers"
	}
	return "instance workers"
}

// paritySREInvestigator reports whether the investigator loop runs on the
// runtime's own worker group.
func paritySREInvestigator(rt *databaseRuntime, inst *fleet.DatabaseInstance) string {
	switch {
	case rt == nil || rt.sre == nil:
		return "missing"
	case rt.workers != inst.Workers || !rt.sreStarted:
		return "not on instance workers"
	}
	return "instance workers"
}

func paritySREScope(rt *databaseRuntime) string {
	if rt == nil || rt.sre == nil {
		return "missing"
	}
	scope, ok := rt.sre.Scope()
	if !ok || scope.Validate() != nil {
		return "unbound"
	}
	return "bound"
}

func paritySREProbeRunner(rt *databaseRuntime) string {
	if rt == nil || rt.probes == nil || rt.rcaAdapter == nil {
		return "missing"
	}
	if rt.rcaAdapter.probes != rt.probes {
		return "separate runners"
	}
	return "shared"
}

func paritySREService(rt *databaseRuntime, inst *fleet.DatabaseInstance) string {
	if rt == nil || inst.Investigations == nil || rt.sre == nil {
		return "missing"
	}
	if inst.Investigations.Coordinator() != rt.sre {
		return "other coordinator"
	}
	return "registered"
}

func parityProbesAttached(rt *databaseRuntime) bool {
	return rt != nil && rt.rcaAdapter != nil && rt.rcaAdapter.probes != nil
}

// parityValueSource reports whether the value ledger (D3) reads this
// runtime's own database.
func parityValueSource(inst *fleet.DatabaseInstance) string {
	for _, source := range fleet.ValueSources(fleetMgr) {
		if source.Name != inst.Name {
			continue
		}
		if source.Pool != nil && source.Pool == inst.Pool {
			return "own pool"
		}
		return "other pool"
	}
	return "missing"
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
	base.RCA.Enabled, base.RCA.LockChainIntervalSeconds = true, 3600
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
	inst := fleetMgr.GetInstance(name)
	if inst != nil && inst.Pool != nil {
		// initFleetMultiDB registers the database in sage.databases.
		control := inst.Pool
		t.Cleanup(func() {
			_, _ = control.Exec(context.Background(),
				"DELETE FROM sage.databases WHERE name = $1", name)
		})
	}
	return inst
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
