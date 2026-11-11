package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/rollout"
)

// canaryRecoverTimeout bounds the startup sweep of interrupted canaries.
const canaryRecoverTimeout = 30 * time.Second

// recoverInterruptedCanaries marks the fleet canaries a stopped sidecar
// left running as interrupted (executor-04b) and notifies each one. The
// API server runs it once at startup, after wiring (router construction
// does no database I/O); a canary started meanwhile holds its run lock,
// so the sweep leaves it alone. No run is resumed. A sweep that fails leaves those runs in canary, where
// nothing resumes them either; the next startup sweeps again.
func recoverInterruptedCanaries(svc *rollout.CanaryService, control *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(context.Background(), canaryRecoverTimeout)
	defer cancel()
	runs, err := svc.RecoverInterrupted(ctx)
	if err != nil {
		logError("rollout", "interrupted canaries not marked; they stay in canary "+
			"and are not resumed: %v", err)
		return
	}
	dispatcher := sharedNotifyDispatcher(control)
	if dispatcher == nil {
		return
	}
	for _, evt := range canaryInterruptedEvents(runs) {
		if err := dispatcher.Dispatch(ctx, evt); err != nil {
			logWarn("rollout", "notify %s: %v", evt.Subject, err)
		}
	}
}

// canaryInterruptedEvents is one canary_interrupted event per run.
func canaryInterruptedEvents(runs []rollout.InterruptedRun) []notify.Event {
	events := make([]notify.Event, 0, len(runs))
	for _, r := range runs {
		info := notify.CanaryInterrupted{RunID: r.Run.EvidenceID, Source: r.Run.SourceInstance}
		for _, inst := range r.Changing {
			info.Changing = append(info.Changing, notify.CanaryTargetAction{
				Database: inst.InstanceID, ActionLogID: inst.ActionLogID})
		}
		events = append(events, notify.CanaryInterruptedEvent(info))
	}
	return events
}
