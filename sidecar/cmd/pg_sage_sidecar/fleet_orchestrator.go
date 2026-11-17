package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/ha"
)

// cycleExecutor is the executor cycle the orchestrator drives
// (executor.Executor).
type cycleExecutor interface {
	RunCycle(ctx context.Context, isReplica bool)
}

// planTicker is the database's plan engine (changeplan.Engine, plan-02a).
type planTicker interface {
	Tick(ctx context.Context) error
}

// retentionRunner is the per-instance retention cleaner (retention.Cleaner).
type retentionRunner interface {
	Run(ctx context.Context)
}

// fleetCycleDeps are the per-database workers one orchestrator cycle drives.
type fleetCycleDeps struct {
	name string
	// ha is the database's one HA monitor (ha_loop.go).
	ha      *ha.Monitor
	exec    cycleExecutor
	plans   planTicker // nil: no plan engine
	brief   *briefing.Worker
	cleaner retentionRunner
	// interval is the cycle period, fixed when the runtime is built so the
	// worker never reads the process config concurrently with its writers.
	interval time.Duration
}

// fleetDBOrchestrator runs executor, briefing, and retention cycles for a
// single fleet database. The ctx is the per-instance context so that
// RemoveInstance can terminate this orchestrator without shutting down the
// fleet. EmergencyStop only blocks action execution; monitoring continues.
func fleetDBOrchestrator(ctx context.Context, deps fleetCycleDeps) {
	ticker := time.NewTicker(deps.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if shutdownCtx != nil && shutdownCtx.Err() != nil {
				return
			}
			runFleetDBCycle(ctx, deps)
		case <-ctx.Done():
			return
		}
	}
}

// runFleetDBCycle is one orchestrator tick: executor (HA-gated: the
// database's monitor is checked just before the cycle, which is told
// isReplica on a replica, an unknown role, safe mode or inside the
// failover cooldown), scheduled
// briefing, retention (G5-B08: fleet and meta databases used to grow
// sage.* forever), then status and health history. Each step is one run
// of its loop in the process loop tracker (observability-06).
func runFleetDBCycle(ctx context.Context, deps fleetCycleDeps) {
	if deps.exec != nil && deps.ha != nil {
		runExecutorCycle(ctx, deps)
	}
	if deps.brief != nil && deps.brief.ShouldRun(time.Now()) {
		runBriefing(ctx, deps.name, deps.brief)
	}
	if deps.cleaner != nil {
		runRetention(ctx, deps.cleaner)
	}
	if fleetMgr == nil {
		return
	}
	if inst := fleetMgr.GetInstance(deps.name); inst != nil {
		updateInstanceFindings(ctx, inst)
		fleetMgr.RecordHealthSnapshot(ctx, deps.name)
	}
}

// runExecutorCycle runs one HA-gated executor cycle, then one plan engine
// tick. The executor logs each step's failure and carries on; it has no
// cycle-level failure, and a plan tick's failure is logged.
func runExecutorCycle(ctx context.Context, deps fleetCycleDeps) {
	isReplica := checkHA(ctx, deps.ha)
	if deps.ha.Snapshot().SafeMode {
		logWarn("ha", "[%s] safe mode active — "+
			"suppressing autonomous actions", deps.name)
	}
	notWritable := isReplica || haNotWritable(deps.ha)
	deps.exec.RunCycle(ctx, notWritable)
	// Plans change the database: only a writable primary advances them.
	if deps.plans == nil || notWritable {
		return
	}
	if err := deps.plans.Tick(ctx); err != nil {
		logWarn("plan", "[%s] plan engine: %v", deps.name, err)
	}
}

// runRetention runs the retention cleaner. It logs and survives each
// table's failure; it has no cycle-level failure to report. Its run is
// tracked by the cleaner (footprint-07).
func runRetention(ctx context.Context, cleaner retentionRunner) {
	cleaner.Run(ctx)
}

// runBriefing generates, stores and dispatches the scheduled briefing
// (llm-06); GenerateBriefing records the run, a failed generation as a
// failed run of the briefing loop (observability-06).
func runBriefing(ctx context.Context, name string, brief *briefing.Worker) {
	if err := brief.Run(ctx); err != nil {
		logWarn("briefing", "[%s] generation failed: %v", name, err)
	}
}
