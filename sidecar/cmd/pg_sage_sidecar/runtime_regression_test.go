package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/startup"
)

// llmTestGlobals configures an LLM-enabled fleet runtime whose provider is
// an unroutable address; no test in this file performs a Chat call.
func llmTestGlobals(t *testing.T) {
	t.Helper()
	preserveFleetRuntimeGlobals(t)
	oldCtx := shutdownCtx
	ctx, cancel := context.WithCancel(context.Background())
	shutdownCtx = ctx
	t.Cleanup(func() { cancel(); shutdownCtx = oldCtx })
	cfg = config.DefaultConfig()
	cfg.LLM.Enabled = true
	cfg.LLM.Endpoint = "http://127.0.0.1:1/v1/chat/completions"
	cfg.LLM.APIKey = "fixture-key-never-used"
	cfg.LLM.Model = "model-one"
	cfg.LLM.OptimizerLLM.Enabled = true
	cfg.LLM.OptimizerLLM.Model = ""
	cfg.LLM.Optimizer.Enabled = false
	cfg.Advisor.Enabled = false
	cfg.Tuner.Enabled = false
	fleetLLMBudget = nil
	configController = config.NewConfigControllerAtGeneration(cfg, 1, nil)
	llmClient = llm.New(&cfg.LLM, nil)
	registerLLMConfigOwner()
}

func applyLLMChange(t *testing.T, mutate func(*config.LLMConfig)) {
	t.Helper()
	candidate := config.Clone(configController.Desired().Config)
	mutate(&candidate.LLM)
	result, err := configController.Apply(
		context.Background(), configController.Desired().Generation, candidate,
	)
	if err != nil {
		t.Fatalf("apply llm change: %v", err)
	}
	if len(result.Applied) == 0 {
		t.Fatalf("llm change was not applied live: %+v", result)
	}
}

// G5-B04 / G3-B02: disabling the LLM must reach every client that can egress.
func TestLLMDisableReachesFleetAndOptimizerClients(t *testing.T) {
	llmTestGlobals(t)
	dbClient, manager := newFleetDBLLMClients("orders", true)
	if dbClient == nil || manager == nil || manager.Optimizer == nil {
		t.Fatal("fleet LLM runtime was not constructed")
	}
	if !dbClient.IsEnabled() || !manager.Optimizer.IsEnabled() {
		t.Fatal("precondition: per-database clients start enabled")
	}

	applyLLMChange(t, func(l *config.LLMConfig) { l.Enabled = false })

	if llmClient.IsEnabled() {
		t.Fatal("shared client still enabled")
	}
	if dbClient.IsEnabled() {
		t.Fatal("per-database general client still enabled after llm.enabled=false")
	}
	if manager.Optimizer.IsEnabled() {
		t.Fatal("per-database optimizer client still enabled after llm.enabled=false")
	}
}

// G5-B04 scenario B: a rotated model/key must reach existing and new clients.
func TestLLMRotationReachesExistingAndLaterClients(t *testing.T) {
	llmTestGlobals(t)
	before, beforeMgr := newFleetDBLLMClients("a", true)

	applyLLMChange(t, func(l *config.LLMConfig) { l.Model = "model-two" })

	if got := before.Model(); got != "model-two" {
		t.Fatalf("existing per-database client model = %q, want model-two", got)
	}
	if got := beforeMgr.Optimizer.Model(); got != "model-two" {
		t.Fatalf("existing optimizer client model = %q, want model-two", got)
	}
	after, _ := newFleetDBLLMClients("b", true)
	if got := after.Model(); got != "model-two" {
		t.Fatalf("client built after rotation model = %q, want model-two", got)
	}
}

// G5-B06 / G3-B03: databases added at runtime must receive a budget share.
func TestFleetBudgetAdmitsDatabaseAddedAfterStartup(t *testing.T) {
	llmTestGlobals(t)
	cfg.LLM.FleetTokenBudgetDaily = 1000
	initializeFleetBudget(nil)

	newFleetDBLLMClients("added-later", true)

	if !fleetLLMBudget.CanSpend("added-later", 10) {
		t.Fatal("database added after startup has no LLM budget allocation")
	}
	if got := fleetLLMBudget.Allocation("added-later"); got != 1000 {
		t.Fatalf("allocation = %d, want 1000", got)
	}
}

// G5-B07: databases[].llm_enabled=false must never build an enabled client.
func TestFleetPerDatabaseLLMDisabledIsEnforced(t *testing.T) {
	llmTestGlobals(t)
	client, manager := newFleetDBLLMClients("pii", false)
	if client.IsEnabled() || manager.ForPurpose("query_tuning").IsEnabled() {
		t.Fatal("llm_enabled=false database received an enabled LLM client")
	}

	applyLLMChange(t, func(l *config.LLMConfig) { l.Model = "model-two" })

	if client.IsEnabled() || manager.ForPurpose("query_tuning").IsEnabled() {
		t.Fatal("reconfigure re-enabled a database whose llm_enabled is false")
	}
	allowed, _ := newFleetDBLLMClients("public", true)
	if !allowed.IsEnabled() {
		t.Fatal("llm_enabled default must keep LLM on")
	}
}

func TestDurationUntilNextUTCMidnight(t *testing.T) {
	loc := time.FixedZone("UTC+5", 5*3600)
	now := time.Date(2026, 9, 26, 22, 30, 0, 0, time.UTC).In(loc)
	if got := durationUntilNextUTCMidnight(now); got != 90*time.Minute {
		t.Fatalf("wait = %s, want 1h30m", got)
	}
	exact := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	if got := durationUntilNextUTCMidnight(exact); got != 24*time.Hour {
		t.Fatalf("wait at midnight = %s, want 24h", got)
	}
}

type countingCleaner struct {
	mu   sync.Mutex
	runs int
}

func (c *countingCleaner) Run(context.Context) {
	c.mu.Lock()
	c.runs++
	c.mu.Unlock()
}

// G5-B08 / C08 / G1-B04: every fleet instance cycle runs retention.
func TestFleetCycleRunsRetentionForInstance(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	fleetMgr = nil
	first, second := &countingCleaner{}, &countingCleaner{}

	runFleetDBCycle(context.Background(), fleetCycleDeps{name: "a", cleaner: first})
	runFleetDBCycle(context.Background(), fleetCycleDeps{name: "b", cleaner: second})
	runFleetDBCycle(context.Background(), fleetCycleDeps{name: "b", cleaner: second})

	if first.runs != 1 || second.runs != 2 {
		t.Fatalf("retention runs a=%d b=%d, want 1 and 2", first.runs, second.runs)
	}
}

// G5-B13 / G1-B07: capability flags come from the instance's own checks.
func TestInstanceRuntimeConfigCarriesCapabilityFlags(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = config.DefaultConfig()
	checks := &startup.CheckResult{
		PGVersionNum: 170002, HasWALColumns: true, HasPlanTimeColumns: true,
	}

	runtimeCfg := instanceRuntimeConfig(checks)

	if runtimeCfg == cfg {
		t.Fatal("instance runtime config must be a clone, not the global")
	}
	if runtimeCfg.PGVersionNum != 170002 || !runtimeCfg.HasWALColumns ||
		!runtimeCfg.HasPlanTimeColumns {
		t.Fatalf("instance flags = %d/%v/%v", runtimeCfg.PGVersionNum,
			runtimeCfg.HasWALColumns, runtimeCfg.HasPlanTimeColumns)
	}
	if cfg.HasWALColumns || cfg.PGVersionNum != 0 {
		t.Fatal("instance checks leaked into the global config")
	}
}

// G5-B12: a database that stops answering must stop reporting healthy.
func TestUpdateInstanceFindingsMarksUnreachableDatabaseDown(t *testing.T) {
	deadPool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(deadPool.Close)
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	inst := &fleet.DatabaseInstance{
		Name: "gone", Pool: deadPool, Config: config.DatabaseConfig{Name: "gone"},
		Status: &fleet.InstanceStatus{Connected: true, PGVersion: "17.2"},
	}
	mgr.RegisterInstance(inst)

	updateInstanceFindings(context.Background(), inst)

	snap := inst.SnapshotStatus()
	if snap.Connected || snap.Error == "" {
		t.Fatalf("status connected=%v error=%q, want disconnected with error",
			snap.Connected, snap.Error)
	}
	overview := mgr.FleetStatus()
	if overview.Summary.Degraded != 1 || overview.Databases[0].Status.HealthScore != 0 {
		t.Fatalf("overview degraded=%d health=%d, want 1 and 0",
			overview.Summary.Degraded, overview.Databases[0].Status.HealthScore)
	}
}

// G5-B10 / G7-B05: notification rules live in the control database.
func TestNotificationControlPoolPrefersMetaDatabase(t *testing.T) {
	meta, primary := &pgxpool.Pool{}, &pgxpool.Pool{}
	if got := notificationControlPool(&metaDBState{Pool: meta}, primary); got != meta {
		t.Fatal("meta mode must read notification rules from the meta database")
	}
	if got := notificationControlPool(nil, primary); got != primary {
		t.Fatal("YAML fleet must read notification rules from the primary pool")
	}
	first := sharedNotifyDispatcher(primary)
	if first == nil || sharedNotifyDispatcher(primary) != first {
		t.Fatal("instances must share one dispatcher per control pool")
	}
	if sharedNotifyDispatcher(nil) != nil {
		t.Fatal("no control pool must yield no dispatcher")
	}
}

// G5-B09: restart is only offered when a supervisor is declared.
func TestRestartRequiresDeclaredSupervisor(t *testing.T) {
	cases := map[string]bool{"": false, "0": false, "false": false,
		"1": true, "true": true, "yes": false}
	for value, want := range cases {
		getenv := func(string) string { return value }
		if got := supervisorDeclared(getenv); got != want {
			t.Errorf("SAGE_SUPERVISED=%q supervised=%v, want %v", value, got, want)
		}
	}
}

func TestForcedShutdownExitCodeHonoursRestart(t *testing.T) {
	restartRequested.Store(false)
	t.Cleanup(func() { restartRequested.Store(false) })
	if got := forcedShutdownExitCode(); got != 1 {
		t.Fatalf("plain forced exit = %d, want 1", got)
	}
	restartRequested.Store(true)
	if got := forcedShutdownExitCode(); got != restartExitCode {
		t.Fatalf("forced exit during restart = %d, want %d", got, restartExitCode)
	}
}

// G10-B11: the mode gauge must distinguish fleet from the meta-db mode.
// Meta keeps 0, the value meta-db deployments reported under the removed
// "extension" label, so existing dashboards keep working.
func TestModeGaugeValue(t *testing.T) {
	want := map[string]int{"meta": 0, "standalone": 1, "fleet": 2}
	for mode, value := range want {
		if got := modeGaugeValue(mode); got != value {
			t.Errorf("modeGaugeValue(%q) = %d, want %d", mode, got, value)
		}
	}
	var b strings.Builder
	writeModeMetric(&b, "fleet")
	if !strings.Contains(b.String(), "pg_sage_mode 2") ||
		!strings.Contains(b.String(), "0=meta, 1=standalone, 2=fleet") {
		t.Fatalf("mode metric output:\n%s", b.String())
	}
}

// G3-B14: per-database fleet budget spend is visible to operators.
func TestFleetBudgetMetricsExposePerDatabaseSpend(t *testing.T) {
	budget := fleet.NewBudget(100, []string{"orders", "billing"})
	budget.Spend("orders", 12)
	var b strings.Builder

	writeFleetBudgetMetrics(&b, budget)

	out := b.String()
	for _, line := range []string{
		`pg_sage_llm_fleet_budget_used_tokens{database="orders"} 12`,
		`pg_sage_llm_fleet_budget_allocation_tokens{database="billing"} 50`,
	} {
		if !strings.Contains(out, line) {
			t.Errorf("metrics missing %q:\n%s", line, out)
		}
	}
	var empty strings.Builder
	writeFleetBudgetMetrics(&empty, nil)
	if empty.Len() != 0 {
		t.Fatalf("nil budget wrote metrics: %q", empty.String())
	}
}

// G7-B09: alerting must be constructed for fleet instances when enabled.
func TestInstanceAlertManagerFollowsAlertingFlag(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = config.DefaultConfig()
	cfg.Alerting.Enabled = false
	if newInstanceAlertManager(&pgxpool.Pool{}) != nil {
		t.Fatal("disabled alerting built a manager")
	}
	cfg.Alerting.Enabled = true
	if newInstanceAlertManager(&pgxpool.Pool{}) == nil {
		t.Fatal("enabled alerting was silently skipped for a fleet instance")
	}
	if newInstanceAlertManager(nil) != nil {
		t.Fatal("instance without a pool must not build an alert manager")
	}
}
