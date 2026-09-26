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
	ctx context.Context, inst *DatabaseInstance, stopped bool,
) error {
	if inst.Pool == nil {
		return fmt.Errorf("no connection pool")
	}
	return executor.SetEmergencyStop(ctx, inst.Pool, stopped)
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
	name string, stopped bool,
) (int, error) {
	targets, err := m.emergencyTargets(name)
	if err != nil {
		return 0, err
	}
	if stopped {
		return m.stopTargets(targets)
	}
	return m.resumeTargets(targets)
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

// stopTargets latches every target in memory first (gating its executor),
// then persists the durable flag outside the manager lock. A persistence
// failure never un-stops a database.
func (m *DatabaseManager) stopTargets(targets []*DatabaseInstance) (int, error) {
	changed := 0
	m.mu.Lock()
	for _, inst := range targets {
		if !inst.Stopped {
			inst.Stopped = true
			changed++
			log.Printf("fleet: %s: emergency stop", inst.Name)
		}
		applyExecutorStopGate(inst, true)
	}
	m.mu.Unlock()
	failed := m.persistTargets(targets, true)
	return changed, emergencyStopResult(true, failed)
}

// resumeTargets clears the durable flag first and releases the in-memory
// latch only for databases whose flag was written.
func (m *DatabaseManager) resumeTargets(targets []*DatabaseInstance) (int, error) {
	failed := m.persistTargets(targets, false)
	changed := 0
	m.mu.Lock()
	for _, inst := range targets {
		if failed[inst.Name] != nil {
			continue
		}
		if inst.Stopped {
			inst.Stopped = false
			changed++
			log.Printf("fleet: %s: resumed", inst.Name)
		}
		applyExecutorStopGate(inst, false)
	}
	m.mu.Unlock()
	return changed, emergencyStopResult(false, failed)
}

// persistTargets reconciles the persisted flag even when the in-memory state
// already matches: after a restart the latch resets while sage.config may
// still say "stopped". Instances without an executor (agent databases,
// failed registrations) have nothing that reads the flag and are skipped.
func (m *DatabaseManager) persistTargets(
	targets []*DatabaseInstance, stopped bool,
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
		err := persist(ctx, inst, stopped)
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
	applyExecutorStopGate(candidate, true)
}
