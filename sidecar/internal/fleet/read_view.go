package fleet

import "errors"

// ErrReadOnlyView is a mutation attempted through a ReadView.
var ErrReadOnlyView = errors.New("fleet read view: changes must go through the fleet " +
	"manager, not a database-scoped view")

// ReadView returns the fleet as a database-scoped reader may see it
// (apiauth-08b): a manager holding only the instances named in visible,
// sharing the live instances, the config, the budget source and the
// fleet stop latch. A name with no instance (a grant on a removed
// database) adds nothing. The view is a snapshot of membership taken
// now, for one request. It is read-only: registration, lifecycle changes
// and emergency stop or resume through it fail with ErrReadOnlyView (or
// do nothing), and never reach m. A nil m yields nil.
func (m *DatabaseManager) ReadView(visible map[string]bool) *DatabaseManager {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v := &DatabaseManager{
		instances: make(map[string]*DatabaseInstance, len(visible)),
		cfg:       m.cfg,
		lifecycle: make(chan struct{}, 1),
		fleetStop: m.fleetStop,
		budget:    m.budget,
		readOnly:  true,
	}
	for name, inst := range m.instances {
		if visible[name] {
			v.instances[name] = inst
		}
	}
	if _, ok := v.instances[m.primaryName]; ok {
		v.primaryName = m.primaryName
	} else {
		v.primaryName = v.firstConnectedNameLocked()
	}
	return v
}
