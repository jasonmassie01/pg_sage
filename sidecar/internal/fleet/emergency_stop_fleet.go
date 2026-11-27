package fleet

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
)

// ErrFleetStopped refuses a per-database resume while a stop of the whole
// fleet holds: only a resume of all databases lifts it.
var ErrFleetStopped = errors.New("fleet-wide stop is active: resume all first")

// fleetStopState is the fleet-wide emergency stop latch. While Stopped,
// every database registered, published or replaced is stopped too.
// Guarded by the manager lock.
type fleetStopState struct {
	Stopped bool
	By      string
	At      time.Time
	// pending counts stops of all databases not yet written; an
	// overlapping resume of all must not lift the latch.
	pending int
}

// stopScope says which emergency_stop_scope a flag write carries.
type stopScope int

const (
	scopeKeep  stopScope = iota // single database: leave the scope row
	scopeFleet                  // write 'fleet' after the flag
	scopeClear                  // write 'database' before clearing the flag
)

// StopResult reports an emergency stop: how many databases changed state
// and, per database, the action_log ids of the running actions it
// cancelled and any cancel it could not confirm.
type StopResult struct {
	Changed        int
	Cancelled      map[string][]int64
	CancelFailures map[string]string
}

// EmergencyStopDetailed stops one database (or all when name is "") and
// cancels the actions each one was running.
func (m *DatabaseManager) EmergencyStopDetailed(name, actor string) (StopResult, error) {
	return m.setEmergencyStopped(name, true, actor)
}

// UseFleetStopStore makes the fleet latch durable in the meta database and
// restores it. A read error fails closed: the fleet restores as stopped by
// the system, like an unreadable per-database flag.
func (m *DatabaseManager) UseFleetStopStore(store FleetStopStore) {
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), emergencyStopPersistTimeout)
	state, err := store.ReadFleetStop(ctx)
	cancel()
	if err != nil {
		log.Printf("fleet: %s unreadable, restoring the fleet as stopped: %v",
			FleetStopKey, err)
		state = executor.EmergencyStopState{
			Stopped: true, UpdatedBy: executor.EmergencyStopActorSystem, UpdatedAt: time.Now(),
		}
	}
	var pending []*DatabaseInstance
	m.mu.Lock()
	m.fleetStore = store
	if state.Stopped {
		pending = m.setFleetLatchLocked(state.UpdatedBy, state.UpdatedAt, nil)
	}
	m.mu.Unlock()
	m.persistInheritedStops(pending)
}

// setFleetLatchLocked turns the fleet latch on with the given attribution
// and latches every published database except skip. It returns the
// databases whose durable flag must now be written. Caller holds m.mu.
func (m *DatabaseManager) setFleetLatchLocked(
	by string, at time.Time, skip *DatabaseInstance,
) []*DatabaseInstance {
	if by == "" {
		by = executor.EmergencyStopActorSystem
	}
	if at.IsZero() {
		at = time.Now()
	}
	m.fleetStop.Stopped, m.fleetStop.By, m.fleetStop.At = true, by, at
	log.Printf("fleet: fleet-wide emergency stop by %s is active", by)
	var pending []*DatabaseInstance
	for _, inst := range m.instances {
		if inst != skip && m.latchForFleetLocked(inst) {
			pending = append(pending, inst)
		}
	}
	return pending
}

// applyFleetLatchLocked runs as inst is published. A runtime restored with
// a fleet-scoped stop turns the latch on for the whole fleet; while the
// latch holds, inst is stopped too. queued says inst already owes a durable
// write (an inherited stop). It returns every database whose durable flag
// must be written once m.mu is released. Caller holds m.mu.
func (m *DatabaseManager) applyFleetLatchLocked(
	inst *DatabaseInstance, queued bool,
) []*DatabaseInstance {
	var pending []*DatabaseInstance
	if queued {
		pending = append(pending, inst)
	}
	if inst.restoredFleet && !m.fleetStop.Stopped {
		pending = append(pending, m.setFleetLatchLocked(inst.StoppedBy, inst.StoppedAt, inst)...)
		return pending
	}
	if m.fleetStop.Stopped && !queued && m.latchForFleetLocked(inst) {
		pending = append(pending, inst)
	}
	return pending
}

// latchForFleetLocked stops a running database on behalf of the fleet
// latch. It reports whether a durable write is now owed (the database has
// an executor that reads the flag); the pending mark keeps an overlapping
// resume from releasing it first. A database already stopped keeps its own
// attribution. Caller holds m.mu.
func (m *DatabaseManager) latchForFleetLocked(inst *DatabaseInstance) bool {
	if inst == nil || inst.Stopped {
		return false
	}
	latchLocked(inst, m.fleetStop.By, m.fleetStop.At)
	log.Printf("fleet: %s: stopped by the fleet-wide emergency stop of %s",
		inst.Name, m.fleetStop.By)
	if inst.Executor == nil {
		return false
	}
	inst.stopsPending++
	return true
}

// latchLocked sets the in-memory stop and closes the executor gate. It
// reports whether the database was running. Caller holds m.mu.
func latchLocked(inst *DatabaseInstance, actor string, at time.Time) bool {
	changed := !inst.Stopped
	inst.Stopped = true
	inst.StoppedBy = actor
	inst.StoppedAt = at
	applyExecutorStopGate(inst, true)
	return changed
}

// fleetResumeGate refuses a per-database resume while the fleet latch
// holds. The caller holds m.transitionMu, so a stop of all that latched
// before this check is seen, and one that latches after it marks every
// target pending.
func (m *DatabaseManager) fleetResumeGate(name string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.fleetStop.Stopped {
		return fmt.Errorf("resuming %s: %w (stopped by %s)", name, ErrFleetStopped,
			m.fleetStop.By)
	}
	return nil
}

// persistFleetKey writes the meta database's fleet latch, when there is
// one, and records a failure under FleetStopKey. Caller holds
// m.transitionMu.
func (m *DatabaseManager) persistFleetKey(stopped bool, actor string, failed map[string]error) {
	m.mu.RLock()
	store := m.fleetStore
	m.mu.RUnlock()
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), emergencyStopPersistTimeout)
	defer cancel()
	if err := store.WriteFleetStop(ctx, stopped, actor); err != nil {
		failed[FleetStopKey] = fmt.Errorf("persisting the fleet-wide stop "+
			"to the meta database: %w", err)
	}
}

// cancelTargets halts the running actions of every stopped database in
// parallel. It runs without m.mu: a cancel waits for statements to end.
func (m *DatabaseManager) cancelTargets(targets []*DatabaseInstance, result *StopResult) {
	result.Cancelled = map[string][]int64{}
	result.CancelFailures = map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, inst := range targets {
		if inst.Executor == nil {
			continue
		}
		wg.Add(1)
		go func(inst *DatabaseInstance) {
			defer wg.Done()
			ids, err := m.cancelOne(inst)
			mu.Lock()
			defer mu.Unlock()
			result.Cancelled[inst.Name] = ids
			if err != nil {
				result.CancelFailures[inst.Name] = err.Error()
			}
		}(inst)
	}
	wg.Wait()
}

// cancelOne cancels one database's running actions and logs the outcome.
func (m *DatabaseManager) cancelOne(inst *DatabaseInstance) ([]int64, error) {
	cancel := m.cancelInFlight
	if cancel == nil {
		cancel = func(inst *DatabaseInstance) ([]int64, error) {
			return inst.Executor.CancelInFlight("emergency stop")
		}
	}
	ids, err := cancel(inst)
	if ids == nil {
		ids = []int64{}
	}
	if len(ids) > 0 {
		log.Printf("fleet: %s: emergency stop cancelled actions %v", inst.Name, ids)
	}
	if err != nil {
		log.Printf("fleet: %s: emergency stop could not confirm every cancel: %v",
			inst.Name, err)
	}
	return ids, err
}

// FleetStopped reports whether a stop of the whole fleet holds (the
// fleet-wide latch); a nil manager is never stopped.
func (m *DatabaseManager) FleetStopped() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.fleetStop.Stopped
}
