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
// so the sweep leaves it alone. No run is resumed. A sweep that fails
// leaves those runs in canary, where nothing resumes them either; the
// next startup sweeps again.
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
		events = append(events, notify.CanaryInterruptedEvent(notify.CanaryInterrupted{
			RunID: r.Run.EvidenceID, Source: r.Run.SourceInstance,
			Changing: canaryTargetActions(r.Changing), Kept: canaryTargetActions(r.Kept)}))
	}
	return events
}

// canaryTargetActions are the instances' databases and action ids.
func canaryTargetActions(instances []rollout.InstanceRecord) []notify.CanaryTargetAction {
	var out []notify.CanaryTargetAction
	for _, inst := range instances {
		out = append(out, notify.CanaryTargetAction{Database: inst.InstanceID,
			ActionLogID: inst.ActionLogID})
	}
	return out
}
