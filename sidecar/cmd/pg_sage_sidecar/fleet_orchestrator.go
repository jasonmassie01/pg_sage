package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/ha"
)

// retentionRunner is the per-instance retention cleaner (retention.Cleaner).
type retentionRunner interface {
	Run(ctx context.Context)
}

// fleetCycleDeps are the per-database workers one orchestrator cycle drives.
type fleetCycleDeps struct {
	name    string
	pool    *pgxpool.Pool
	ha      *ha.Monitor
	exec    *executor.Executor
	brief   *briefing.Worker
	cleaner retentionRunner
}

// fleetDBOrchestrator runs executor, briefing, and retention cycles for a
// single fleet database. The ctx is the per-instance context so that
// RemoveInstance can terminate this orchestrator without shutting down the
// fleet. EmergencyStop only blocks action execution; monitoring continues.
func fleetDBOrchestrator(ctx context.Context, deps fleetCycleDeps) {
	ticker := time.NewTicker(cfg.Analyzer.Interval() + 5*time.Second)
	defer ticker.Stop()

	// Per-database HA monitor so fleet executors gate autonomous actions
	// on this DB's own replica/safe-mode state (S2/S3).
	if deps.pool != nil {
		deps.ha = ha.New(deps.pool, logStructuredWrapper)
	}
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

// runFleetDBCycle is one orchestrator tick: executor (HA-gated), scheduled
// briefing, retention (G5-B08: fleet and meta databases used to grow
// sage.* forever), then status and health history.
func runFleetDBCycle(ctx context.Context, deps fleetCycleDeps) {
	if deps.exec != nil && deps.ha != nil {
		isReplica := deps.ha.Check(ctx)
		if deps.ha.InSafeMode() {
			logWarn("ha", "[%s] safe mode active — "+
				"suppressing autonomous actions", deps.name)
			isReplica = true
		}
		deps.exec.RunCycle(ctx, isReplica)
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
