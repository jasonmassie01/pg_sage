package main

import (
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/retention"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
)

// startExecution builds the executor behind the standing policy gate, then
// the workers that act on it: provider observability, continuous autonomy,
// action expiry and the HA-gated orchestrator (executor, briefing and
// retention cycles).
func (rt *databaseRuntime) startExecution() {
	rt.buildExecutor()
	rt.startAutonomyLoops()
	startProviderObservability(
		rt.ctx, rt.workers, rt.spec.Pool, rt.cfg, rt.executor, rt.rca,
	)
	// Autonomous index builds earn load admission from pg-side IO (D6).
	if startIOAdmission(rt.ctx, rt.workers, rt.spec.Pool, rt.cfg,
		rt.spec.Name, rt.executor) != nil {
		rt.note("io_admission")
	}
	if err := startInstanceAutonomy(
		rt.ctx, rt.workers, rt.spec.Pool, rt.cfg, rt.spec.Name, rt.executor,
	); err != nil {
		logError(rt.spec.Scope, "db %q: continuous autonomy unavailable: %v",
			rt.spec.Name, err)
	} else {
		rt.note("autonomy")
	}
	if rt.rca != nil {
		rt.rca.WithActionStore(rt.actions)
		if rt.dispatcher != nil {
			rt.rca.WithDispatcher(rt.dispatcher)
		}
	}
	rt.start(func() {
		store.StartActionExpiry(rt.ctx, rt.actions, logStructuredWrapper)
	})
	rt.brief = briefing.New(rt.spec.Pool, cfg, rt.generalLLM, logStructuredWrapper)
	deps := fleetCycleDeps{
		name: rt.spec.Name, pool: rt.spec.Pool, exec: rt.executor,
		brief: rt.brief, cleaner: retention.New(rt.spec.Pool, cfg, logStructuredWrapper).
			WithControlPool(rt.spec.ControlPool),
		interval: cfg.Analyzer.Interval() + 5*time.Second,
	}
	rt.start(func() { fleetDBOrchestrator(rt.ctx, deps) })
	rt.note("briefing+retention")
}

// buildExecutor wires the executor identically in every mode: ramp start,
// ANALYZE semaphore, managed-config adapter, action store and execution
// mode, the standing policy gate (the only authority), per-database trust
// and executor gates, notifications and justification.
func (rt *databaseRuntime) buildExecutor() {
	rampStart, err := schema.PersistTrustRampStart(rt.ctx, rt.spec.Pool, configRampStart)
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: trust ramp start: %v", rt.spec.Name, err)
		rampStart = time.Now()
	}
	ex := executor.New(rt.spec.Pool, rt.cfg, rampStart, logStructuredWrapper)
	ex.WithAnalyzeSemaphore(ensureAnalyzeSemaphore())
	installAzureManagedConfig(ex, cfg, rt.provider,
		rt.spec.Pool.Config().ConnConfig.Host, rt.spec.Scope)
	rt.actions = store.NewActionStore(rt.spec.Pool)
	ex.WithActionStore(rt.actions, rt.spec.ExecMode)
	var policyDatabaseID *int
	if rt.spec.DatabaseID > 0 {
		id := rt.spec.DatabaseID
		policyDatabaseID = &id
	}
	if err := ex.SetTrustLevel(rt.spec.Config.TrustLevel); err != nil {
		logWarn(rt.spec.Scope, "db %q: invalid trust level %q: %v",
			rt.spec.Name, rt.spec.Config.TrustLevel, err)
	}
	ex.SetExecutorEnabled(rt.spec.Config.IsExecutorEnabled())
	// Earned autonomy (M7) restricts the gate built just below. It carries
	// over the autonomy this database's settings (applied above) grant.
	rt.installAutonomy(ex)
	if err := ex.EnableStandingPolicyWithStore(
		rt.ctx, rt.spec.ControlPool, cfg.Policy.Profile, policyDatabaseID,
	); err != nil {
		logError(rt.spec.Scope, "db %q: standing policy unavailable; executor is "+
			"fail-closed: %v", rt.spec.Name, err)
	}
	if rt.dispatcher != nil {
		ex.WithDispatcher(rt.dispatcher)
	}
	ex.WithDatabaseName(rt.spec.Name)
	if rt.llmOn {
		ex.WithJustifier(rt.generalLLM)
	}
	rt.executor = ex
}

// logExecutorSettings records the executor's safety wiring once the
// runtime is complete, so an operator can compare modes from the logs.
func (rt *databaseRuntime) logExecutorSettings() {
	s := rt.executor.RuntimeSettings()
	logInfo(rt.spec.Scope, "db %q: executor provider=%s trust=%s mode=%s enabled=%t "+
		"gate=%t managed_config=%t io_admission=%t", rt.spec.Name, s.Provider,
		s.TrustLevel, s.ExecutionMode, s.ExecutorEnabled, s.PolicyGate,
		s.ManagedConfig, s.IOEvidence)
}

var analyzeSemMu sync.Mutex

// ensureAnalyzeSemaphore returns the process-wide ANALYZE semaphore,
// sizing it on first use so a runtime never runs without the bound.
func ensureAnalyzeSemaphore() chan struct{} {
	analyzeSemMu.Lock()
	defer analyzeSemMu.Unlock()
	if analyzeSem == nil {
		analyzeSem = newAnalyzeSemaphore()
	}
	return analyzeSem
}
