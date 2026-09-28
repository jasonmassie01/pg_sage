package fleet

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
)

const emergencyStopPersistTimeout = 5 * time.Second

// EmergencyStopError reports databases whose persisted emergency_stop flag
// could not be written. Every targeted database was still stopped (or kept
// stopped, for a resume) in memory: the kill switch fails closed.
type EmergencyStopError struct {
	Stopped bool
	Failed  map[string]error
}

func (e *EmergencyStopError) Error() string {
	names := make([]string, 0, len(e.Failed))
	for name := range e.Failed {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s: %v", name, e.Failed[name]))
	}
	return fmt.Sprintf(
		"emergency stop persistence failed for %d database(s): %s",
		len(names), strings.Join(parts, "; "),
	)
}

// Unwrap exposes each per-database error to errors.Is / errors.As.
func (e *EmergencyStopError) Unwrap() []error {
	errs := make([]error, 0, len(e.Failed))
	for _, err := range e.Failed {
		errs = append(errs, err)
	}
	return errs
}

// persistEmergencyStop writes the durable per-database flag that every
// executor re-reads before acting.
func persistEmergencyStop(
	ctx context.Context, inst *DatabaseInstance, stopped bool, actor string,
) error {
	if inst.Pool == nil {
		return fmt.Errorf("no connection pool")
	}
	return executor.SetEmergencyStop(ctx, inst.Pool, stopped, actor)
}

// InstanceStopped reads the in-memory emergency-stop latch under the
// manager lock.
func (m *DatabaseManager) InstanceStopped(inst *DatabaseInstance) bool {
	if inst == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return inst.Stopped
}

func (m *DatabaseManager) setEmergencyStopped(
	name string, stopped bool, actor string,
) (int, error) {
	if actor == "" {
		actor = executor.EmergencyStopActorSystem
	}
	targets, err := m.emergencyTargets(name)
	if err != nil {
		return 0, err
	}
	if stopped {
		return m.stopTargets(targets, actor)
	}
	return m.resumeTargets(targets, actor)
}

// emergencyTargets snapshots the instances addressed by name ("" = all).
func (m *DatabaseManager) emergencyTargets(
	name string,
) ([]*DatabaseInstance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if name != "" {
		inst, ok := m.instances[name]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrDatabaseNotFound, name)
		}
		return []*DatabaseInstance{inst}, nil
	}
	targets := make([]*DatabaseInstance, 0, len(m.instances))
	for _, inst := range m.instances {
		targets = append(targets, inst)
	}
	return targets, nil
}

// stopTargets latches every target in memory first (gating its executor)
// without waiting for any other transition, then persists the durable flag
// under the transition lock. A persistence failure never un-stops a
// database. The pending mark tells an overlapping resume that this stop
// has not been written yet and must win.
func (m *DatabaseManager) stopTargets(
	targets []*DatabaseInstance, actor string,
) (int, error) {
	changed := 0
	now := time.Now()
	m.mu.Lock()
	for _, inst := range targets {
		if !inst.Stopped {
			changed++
		}
		inst.Stopped = true
		inst.StoppedBy = actor
		inst.StoppedAt = now
		inst.stopsPending++
		applyExecutorStopGate(inst, true)
		log.Printf("fleet: %s: emergency stop by %s", inst.Name, actor)
	}
	m.mu.Unlock()

	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	failed := m.persistTargets(targets, true, actor)
	m.mu.Lock()
	for _, inst := range targets {
		inst.stopsPending--
	}
	m.mu.Unlock()
	return changed, emergencyStopResult(true, failed)
}

// resumeTargets clears the durable flag first and releases the in-memory
// latch only for databases whose flag was written and that no overlapping
// stop has latched since: such a stop persists after this resume, so
// releasing would leave memory running over a durable stop.
func (m *DatabaseManager) resumeTargets(
	targets []*DatabaseInstance, actor string,
) (int, error) {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	failed := m.persistTargets(targets, false, actor)
	changed := 0
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range targets {
		if failed[inst.Name] != nil {
			continue
		}
		if inst.stopsPending > 0 {
			log.Printf("fleet: %s: resume by %s superseded by a concurrent stop",
				inst.Name, actor)
			continue
		}
		if inst.Stopped {
			changed++
			log.Printf("fleet: %s: resumed by %s", inst.Name, actor)
		}
		inst.Stopped = false
		inst.StoppedBy = ""
		inst.StoppedAt = time.Time{}
		applyExecutorStopGate(inst, false)
	}
	return changed, emergencyStopResult(false, failed)
}

// persistTargets reconciles the persisted flag even when the in-memory state
// already matches, so a repeated stop or resume re-asserts the durable flag
// and is audited. Instances without an executor (agent databases, failed
// registrations) have nothing that reads the flag and are skipped. The
// caller holds m.transitionMu.
func (m *DatabaseManager) persistTargets(
	targets []*DatabaseInstance, stopped bool, actor string,
) map[string]error {
	persist := m.persistStop
	if persist == nil {
		persist = persistEmergencyStop
	}
	failed := map[string]error{}
	for _, inst := range targets {
		if inst.Executor == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(
			context.Background(), emergencyStopPersistTimeout,
		)
		err := persist(ctx, inst, stopped, actor)
		cancel()
		if err != nil {
			failed[inst.Name] = fmt.Errorf("persisting emergency stop: %w", err)
		}
	}
	return failed
}

func emergencyStopResult(stopped bool, failed map[string]error) error {
	if len(failed) == 0 {
		return nil
	}
	return &EmergencyStopError{Stopped: stopped, Failed: failed}
}

// applyExecutorStopGate is the in-memory kill switch. Resume restores the
// configured per-database executor gate rather than forcing it on.
func applyExecutorStopGate(inst *DatabaseInstance, stopped bool) {
	if inst == nil || inst.Executor == nil {
		return
	}
	if stopped {
		inst.Executor.SetExecutorEnabled(false)
		return
	}
	inst.Executor.SetExecutorEnabled(inst.Config.IsExecutorEnabled())
}

// inheritEmergencyStop carries the in-memory latch onto a replacement
// runtime. Caller holds m.mu.
func inheritEmergencyStop(old, candidate *DatabaseInstance) {
	if old == nil || candidate == nil || !old.Stopped {
		return
	}
	candidate.Stopped = true
	candidate.StoppedBy = old.StoppedBy
	candidate.StoppedAt = old.StoppedAt
	applyExecutorStopGate(candidate, true)
}
