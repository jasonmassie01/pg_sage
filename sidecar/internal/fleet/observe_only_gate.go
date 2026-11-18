package fleet

import (
	"errors"
	"fmt"
)

// ErrObserveOnlyMember refuses an action on an observe-only member: it has
// no executor, and only full members can have actions (fleetpol-01d).
var ErrObserveOnlyMember = errors.New("observe-only member: it has no executor and " +
	"cannot have actions; promote it to a full member (policy set enrollment.tier: full)")

// ErrActionTargetNotFound is an action on a database the fleet does not run.
var ErrActionTargetNotFound = errors.New("database action executor not found")

// SetObserveOnly records the observe-only member names (the sweeper's
// newest listing); their action requests are refused.
func (m *DatabaseManager) SetObserveOnly(names []string) {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	m.mu.Lock()
	m.observeOnly = set
	m.mu.Unlock()
}

// ActionTarget is the running instance an action on name goes to. An
// observe-only member is refused even when a runtime of the same name is
// still draining, so a light tier never executes.
func (m *DatabaseManager) ActionTarget(name string) (*DatabaseInstance, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.observeOnly[name] {
		return nil, fmt.Errorf("%w: %s", ErrObserveOnlyMember, name)
	}
	inst := m.instances[name]
	if name == "" || inst == nil {
		return nil, fmt.Errorf("%w: %q", ErrActionTargetNotFound, name)
	}
	return inst, nil
}
