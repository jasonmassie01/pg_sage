package fleet

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
)

// readPersistedStop reads the durable flag through the instance pool. An
// instance without a pool has no durable flag (errNoDurableFlag); its
// executor already fails closed on every action (CheckEmergencyStop with
// nil pool).
func readPersistedStop(
	ctx context.Context, inst *DatabaseInstance,
) (executor.EmergencyStopState, error) {
	if inst.Pool == nil {
		return executor.EmergencyStopState{}, errNoDurableFlag
	}
	return executor.ReadEmergencyStop(ctx, inst.Pool)
}

// readStopState reads inst's durable flag with the test hook or the
// sage.config default, bounded by the persist timeout.
func (m *DatabaseManager) readStopState(
	ctx context.Context, inst *DatabaseInstance,
) (executor.EmergencyStopState, error) {
	read := m.readStop
	if read == nil {
		read = readPersistedStop
	}
	ctx, cancel := context.WithTimeout(ctx, emergencyStopPersistTimeout)
	defer cancel()
	return read(ctx, inst)
}

// restoredFleetScope reports whether a restored stop was a stop of the
// whole fleet. A read error keeps the database's own stop and does not
// stop the rest of the fleet.
func (m *DatabaseManager) restoredFleetScope(inst *DatabaseInstance) bool {
	read := m.readScope
	if read == nil {
		read = readMemberScope
	}
	ctx, cancel := context.WithTimeout(context.Background(), emergencyStopPersistTimeout)
	defer cancel()
	fleet, err := read(ctx, inst)
	if err != nil {
		log.Printf("fleet: %s: %s unreadable, restoring its own stop only: %v",
			inst.Name, FleetScopeKey, err)
		return false
	}
	return fleet
}

// restorePersistedStop latches an unpublished runtime from sage.config so a
// restart or reconnect never shows a stopped database as running, and marks
// a stop whose scope is 'fleet' so publishing it re-latches the fleet. A
// read error fails closed, attributed to the system. Instances without an
// executor have no flag and are skipped. The caller must not hold m.mu:
// this performs database I/O before inst is published.
func (m *DatabaseManager) restorePersistedStop(inst *DatabaseInstance) {
	if inst == nil || inst.Executor == nil {
		return
	}
	state, err := m.readStopState(context.Background(), inst)
	if errors.Is(err, errNoDurableFlag) {
		return
	}
	if err != nil {
		log.Printf("fleet: %s: emergency stop state unreadable, "+
			"restoring as stopped: %v", inst.Name, err)
		state = executor.EmergencyStopState{
			Stopped:   true,
			UpdatedBy: executor.EmergencyStopActorSystem,
			UpdatedAt: time.Now(),
		}
	}
	if !state.Stopped {
		return
	}
	if state.UpdatedBy == "" {
		state.UpdatedBy = executor.EmergencyStopActorSystem
	}
	// The runtime's workers already poll InstanceStopped, so the latch is
	// written under m.mu even though inst is not published yet.
	m.mu.Lock()
	inst.Stopped = true
	inst.StoppedBy = state.UpdatedBy
	inst.StoppedAt = state.UpdatedAt
	m.mu.Unlock()
	applyExecutorStopGate(inst, true)
	fleet := false
	if err == nil {
		fleet = m.restoredFleetScope(inst)
		m.mu.Lock()
		inst.restoredFleet = fleet
		m.mu.Unlock()
	}
	log.Printf("fleet: %s: restored emergency stop by %s (fleet-wide: %v)",
		inst.Name, state.UpdatedBy, fleet)
}

// applyStopStatus copies the latch and its attribution into the API view.
// Caller holds m.mu.
func applyStopStatus(ds *DatabaseStatus, inst *DatabaseInstance) {
	ds.EmergencyStopped = inst.Stopped
	if !inst.Stopped {
		return
	}
	ds.EmergencyStoppedBy = inst.StoppedBy
	if !inst.StoppedAt.IsZero() {
		at := inst.StoppedAt
		ds.EmergencyStoppedAt = &at
	}
}

// applyFleetStopStatus copies the fleet latch into the overview summary.
// Caller holds m.mu.
func applyFleetStopStatus(summary *FleetSummary, latch fleetStopState) {
	summary.FleetStopped = latch.Stopped
	if !latch.Stopped {
		return
	}
	summary.FleetStoppedBy = latch.By
	if !latch.At.IsZero() {
		at := latch.At
		summary.FleetStoppedAt = &at
	}
}
