package fleet

import (
	"context"
	"log"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
)

// readPersistedStop reads the durable flag through the instance pool. An
// instance without a pool has no durable flag to restore; its executor
// already fails closed on every action (CheckEmergencyStop with nil pool).
func readPersistedStop(
	ctx context.Context, inst *DatabaseInstance,
) (executor.EmergencyStopState, error) {
	if inst.Pool == nil {
		return executor.EmergencyStopState{}, nil
	}
	return executor.ReadEmergencyStop(ctx, inst.Pool)
}

// restorePersistedStop latches an unpublished runtime from sage.config so a
// restart or reconnect never shows a stopped database as running. A read
// error fails closed, attributed to the system. Instances without an
// executor have no flag and are skipped. The caller must not hold m.mu:
// this performs database I/O before inst is published.
func (m *DatabaseManager) restorePersistedStop(inst *DatabaseInstance) {
	if inst == nil || inst.Executor == nil {
		return
	}
	read := m.readStop
	if read == nil {
		read = readPersistedStop
	}
	ctx, cancel := context.WithTimeout(
		context.Background(), emergencyStopPersistTimeout,
	)
	state, err := read(ctx, inst)
	cancel()
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
	inst.Stopped = true
	inst.StoppedBy = state.UpdatedBy
	inst.StoppedAt = state.UpdatedAt
	applyExecutorStopGate(inst, true)
	log.Printf("fleet: %s: restored emergency stop by %s", inst.Name, inst.StoppedBy)
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
