package fleet

import "context"

// LifecycleMutation is the exclusive publication scope for managed database
// lifecycle changes. Reads remain available while a mutation prepares a new
// runtime, but no other lifecycle writer can change instance identity.
type LifecycleMutation struct {
	manager *DatabaseManager
}

// WithLifecycle serializes managed runtime publication. The callback must not
// call another lifecycle-mutating manager method, because it already owns the
// lifecycle reservation.
func (m *DatabaseManager) WithLifecycle(
	ctx context.Context,
	fn func(*LifecycleMutation) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.lifecycle:
	}
	defer func() { m.lifecycle <- struct{}{} }()
	return fn(&LifecycleMutation{manager: m})
}

// ValidateReplacement reserves the exact active generation and target name.
func (op *LifecycleMutation) ValidateReplacement(
	oldName, newName string,
	expected *DatabaseInstance,
) (*DatabaseInstance, error) {
	m := op.manager
	m.mu.RLock()
	defer m.mu.RUnlock()
	old := m.instances[oldName]
	if old == nil {
		return nil, ErrDatabaseNotFound
	}
	if expected != nil && old != expected {
		return nil, ErrInstanceConflict
	}
	if newName != oldName && m.instances[newName] != nil {
		return nil, replacementConflict(newName)
	}
	return old, nil
}

// PublishReplacement atomically publishes a validated candidate.
func (op *LifecycleMutation) PublishReplacement(
	oldName string,
	old, candidate *DatabaseInstance,
) error {
	return op.manager.commitReplacement(oldName, old, candidate)
}

// ValidateRegistration ensures a create can publish the requested name.
func (op *LifecycleMutation) ValidateRegistration(name string) error {
	op.manager.mu.RLock()
	defer op.manager.mu.RUnlock()
	if name == "" {
		return ErrInvalidInstance
	}
	if op.manager.instances[name] != nil {
		return replacementConflict(name)
	}
	return nil
}

// PublishRegistration publishes a new runtime inside a lifecycle reservation.
func (op *LifecycleMutation) PublishRegistration(
	inst *DatabaseInstance,
) error {
	if inst == nil {
		return ErrInvalidInstance
	}
	m := op.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.instances[inst.Name] != nil {
		return replacementConflict(inst.Name)
	}
	if m.primaryName == "" && inst.Pool != nil {
		m.primaryName = inst.Name
	}
	m.instances[inst.Name] = inst
	return nil
}

// CurrentByDatabaseID finds the current runtime despite a completed rename.
func (op *LifecycleMutation) CurrentByDatabaseID(
	id int,
) *DatabaseInstance {
	op.manager.mu.RLock()
	defer op.manager.mu.RUnlock()
	for _, inst := range op.manager.instances {
		if inst.DatabaseID == id {
			return inst
		}
	}
	return nil
}

// DetachInstance removes the exact generation found by this mutation.
func (op *LifecycleMutation) DetachInstance(
	expected *DatabaseInstance,
) error {
	if expected == nil {
		return nil
	}
	m := op.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, inst := range m.instances {
		if inst != expected {
			continue
		}
		delete(m.instances, name)
		if m.primaryName == name {
			m.primaryName = m.firstConnectedNameLocked()
		}
		return nil
	}
	return ErrInstanceConflict
}
