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
// sage.* forever), then status and health history.
func runFleetDBCycle(ctx context.Context, deps fleetCycleDeps) {
	if deps.exec != nil && deps.ha != nil {
		isReplica := checkHA(ctx, deps.ha)
		if deps.ha.Snapshot().SafeMode {
			logWarn("ha", "[%s] safe mode active — "+
				"suppressing autonomous actions", deps.name)
		}
		deps.exec.RunCycle(ctx, isReplica || haNotWritable(deps.ha))
	}
	if deps.brief != nil && deps.brief.ShouldRun(time.Now()) {
		text, err := deps.brief.Generate(ctx)
		if err != nil {
			logWarn("briefing", "[%s] generation failed: %v", deps.name, err)
		} else {
			deps.brief.Dispatch(ctx, text)
			deps.brief.MarkRan()
		}
	}
	if deps.cleaner != nil {
		deps.cleaner.Run(ctx)
	}
	if fleetMgr == nil {
		return
	}
	if inst := fleetMgr.GetInstance(deps.name); inst != nil {
		updateInstanceFindings(ctx, inst)
		fleetMgr.RecordHealthSnapshot(ctx, deps.name)
	}
}
