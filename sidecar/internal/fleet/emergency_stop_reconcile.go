package fleet

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
)

// StopReconcileInterval is how often the sidecar re-reads every database's
// durable emergency_stop flag. Executors already re-read the flag before
// every action; the reconcile makes the dashboard and the in-flight cancel
// follow a stop or resume made in SQL.
const StopReconcileInterval = 10 * time.Second

type reconcileAction int

const (
	reconcileNone reconcileAction = iota
	reconcileLatched
	reconcileRewrite
)

// RunStopReconcile runs ReconcileStops every interval until ctx is done.
func (m *DatabaseManager) RunStopReconcile(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = StopReconcileInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.ReconcileStops(ctx)
		}
	}
}

// ReconcileStops brings every database's in-memory latch in line with its
// durable flag. A flag that says stopped, or cannot be read, latches the
// database and cancels its running actions. A database is released only
// when its flag was read as 'false', no API stop of it is still being
// written, its own stop was saved, and the fleet latch is off; a
// fleet-latched database whose flag reads 'false' (a per-database SQL
// resume, or the bootstrap seed) has its stop written again.
func (m *DatabaseManager) ReconcileStops(ctx context.Context) {
	for _, inst := range m.reconcileTargets() {
		if ctx.Err() != nil {
			return
		}
		if m.reconcileStop(ctx, inst) {
			m.cancelLatched(inst)
		}
	}
}

// reconcileTargets snapshots the databases with an executor: only they
// read the durable flag.
func (m *DatabaseManager) reconcileTargets() []*DatabaseInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	targets := make([]*DatabaseInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		if inst.Executor != nil {
			targets = append(targets, inst)
		}
	}
	return targets
}

// reconcileStop reconciles one database under the transition lock, so no
// API stop or resume writes its flag between the read and the decision.
// It reports whether the database was latched by this tick.
func (m *DatabaseManager) reconcileStop(ctx context.Context, inst *DatabaseInstance) bool {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	m.retryUnsavedStop(inst)
	state, err := m.readStopState(ctx, inst)
	if errors.Is(err, errNoDurableFlag) {
		return false
	}
	m.mu.Lock()
	action := m.reconcileLocked(inst, state, err)
	m.mu.Unlock()
	if action == reconcileRewrite {
		m.rewriteStop(inst, scopeFleet)
	}
	return action == reconcileLatched
}

// reconcileLocked decides one database's transition. Caller holds m.mu.
func (m *DatabaseManager) reconcileLocked(
	inst *DatabaseInstance, state executor.EmergencyStopState, readErr error,
) reconcileAction {
	if m.instances[inst.Name] != inst {
		return reconcileNone
	}
	if readErr != nil || state.Stopped {
		if inst.Stopped {
			return reconcileNone
		}
		by, at := state.UpdatedBy, state.UpdatedAt
		if readErr != nil {
			log.Printf("fleet: %s: emergency stop state unreadable, stopping: %v",
				inst.Name, readErr)
			by, at = executor.EmergencyStopActorSystem, time.Now()
		}
		if by == "" {
			by = executor.EmergencyStopActorSystem
		}
		if at.IsZero() {
			at = time.Now()
		}
		latchLocked(inst, by, at)
		inst.stopsPending++
		log.Printf("fleet: %s: emergency stop by %s found in sage.config", inst.Name, by)
		return reconcileLatched
	}
	if !inst.Stopped {
		return reconcileNone
	}
	if m.fleetStop.Stopped {
		return reconcileRewrite
	}
	if inst.stopsPending > 0 || inst.stopUnsaved {
		return reconcileNone
	}
	log.Printf("fleet: %s: resumed in sage.config (by %s)", inst.Name, state.UpdatedBy)
	releaseLocked(inst)
	return reconcileNone
}

// retryUnsavedStop writes again a latched stop whose durable flag could not
// be saved. Caller holds m.transitionMu.
func (m *DatabaseManager) retryUnsavedStop(inst *DatabaseInstance) {
	m.mu.RLock()
	unsaved := inst.Stopped && inst.stopUnsaved
	fleetWide := m.fleetStop.Stopped
	m.mu.RUnlock()
	if !unsaved {
		return
	}
	scope := scopeKeep
	if fleetWide {
		scope = scopeFleet
	}
	m.rewriteStop(inst, scope)
}

// rewriteStop persists inst's in-memory stop. Caller holds m.transitionMu.
func (m *DatabaseManager) rewriteStop(inst *DatabaseInstance, scope stopScope) {
	m.mu.RLock()
	actor := inst.StoppedBy
	m.mu.RUnlock()
	if actor == "" {
		actor = executor.EmergencyStopActorSystem
	}
	failed := m.persistTargets([]*DatabaseInstance{inst}, true, actor, scope)
	m.mu.Lock()
	markStopSaved(inst, failed)
	m.mu.Unlock()
	if err := failed[inst.Name]; err != nil {
		log.Printf("fleet: %s: emergency stop not saved, still stopped in memory: %v",
			inst.Name, err)
	}
}

// cancelLatched cancels the running actions of a database the reconcile
// just latched, then drops the pending mark that kept an overlapping
// resume from releasing it mid-cancel.
func (m *DatabaseManager) cancelLatched(inst *DatabaseInstance) {
	_, _ = m.cancelOne(inst)
	m.mu.Lock()
	inst.stopsPending--
	m.mu.Unlock()
}
