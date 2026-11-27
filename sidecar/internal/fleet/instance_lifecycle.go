package fleet

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// RemoveInstance keeps the legacy bool API while bounding its caller.
func (m *DatabaseManager) RemoveInstance(name string) bool {
	ctx, cancel := context.WithTimeout(
		context.Background(), defaultInstanceTeardownTimeout,
	)
	defer cancel()
	err := m.RemoveInstanceContext(ctx, name)
	return !errors.Is(err, ErrDatabaseNotFound)
}

// RemoveInstanceContext detaches under the manager lock, then tears down
// the runtime without retaining that lock.
func (m *DatabaseManager) RemoveInstanceContext(
	ctx context.Context,
	name string,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var inst *DatabaseInstance
	err := m.WithLifecycle(ctx, func(*LifecycleMutation) error {
		var ok bool
		inst, ok = m.detachInstance(name)
		if !ok {
			return ErrDatabaseNotFound
		}
		return nil
	})
	if err != nil {
		return err
	}
	return awaitInstanceTeardown(ctx, inst)
}

// ShutdownInstance drains an unpublished or rejected runtime generation.
// It is primarily used by lifecycle owners that prepared a candidate but
// could not publish it.
func ShutdownInstance(ctx context.Context, inst *DatabaseInstance) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return awaitInstanceTeardown(ctx, inst)
}

func (m *DatabaseManager) detachInstance(
	name string,
) (*DatabaseInstance, bool) {
	m.mu.Lock()
	inst, ok := m.instances[name]
	if ok {
		delete(m.instances, name)
		if m.primaryName == name {
			m.primaryName = m.firstConnectedNameLocked()
		}
	}
	m.mu.Unlock()
	return inst, ok
}

// ReplaceInstance validates a fully built candidate before atomic
// publication, then retires the old generation outside the manager lock.
func (m *DatabaseManager) ReplaceInstance(
	ctx context.Context,
	oldName string,
	candidate *DatabaseInstance,
	healthCheck func(context.Context, *DatabaseInstance) error,
) error {
	return m.ReplaceInstanceIfCurrent(
		ctx, oldName, nil, candidate, healthCheck,
	)
}

// ReplaceInstanceIfCurrent health-checks and publishes candidate only when
// expected is still the active generation. Reconnect callers pass the failed
// instance they observed so a concurrent update cannot be overwritten.
func (m *DatabaseManager) ReplaceInstanceIfCurrent(
	ctx context.Context,
	oldName string,
	expected *DatabaseInstance,
	candidate *DatabaseInstance,
	healthCheck func(context.Context, *DatabaseInstance) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if candidate == nil || candidate.Name == "" || healthCheck == nil {
		return rejectCandidate(ErrInvalidInstance, candidate)
	}
	if err := ctx.Err(); err != nil {
		return rejectCandidate(err, candidate)
	}
	var old *DatabaseInstance
	err := m.WithLifecycle(ctx, func(op *LifecycleMutation) error {
		var validateErr error
		old, validateErr = op.ValidateReplacement(
			oldName, candidate.Name, expected,
		)
		if validateErr != nil {
			return validateErr
		}
		if checkErr := healthCheck(ctx, candidate); checkErr != nil {
			return checkErr
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return op.PublishReplacement(oldName, old, candidate)
	})
	if err != nil {
		return rejectCandidate(err, candidate)
	}
	return awaitInstanceTeardown(ctx, old)
}

// commitReplacement publishes candidate in old's place and returns the
// runtimes whose durable stop must now be written (an inherited stop, or
// the fleet latch).
func (m *DatabaseManager) commitReplacement(
	oldName string,
	old, candidate *DatabaseInstance,
) ([]*DatabaseInstance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.instances[oldName]; current != old {
		return nil, ErrInstanceConflict
	}
	if candidate.Name != oldName {
		if m.instances[candidate.Name] != nil {
			return nil, replacementConflict(candidate.Name)
		}
		delete(m.instances, oldName)
	}
	inherited := inheritEmergencyStop(old, candidate)
	stampRegistered(candidate, time.Now())
	m.instances[candidate.Name] = candidate
	if m.primaryName == oldName {
		m.primaryName = candidate.Name
		if candidate.Pool == nil {
			m.primaryName = m.firstConnectedNameLocked()
		}
	}
	return m.applyFleetLatchLocked(candidate, inherited), nil
}

func replacementConflict(name string) error {
	return fmt.Errorf("%w: target name %q", ErrInstanceConflict, name)
}

func rejectCandidate(err error, candidate *DatabaseInstance) error {
	return errors.Join(err, cleanupRejectedCandidate(candidate))
}

func cleanupRejectedCandidate(candidate *DatabaseInstance) error {
	if candidate == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(
		context.Background(), defaultInstanceTeardownTimeout,
	)
	defer cancel()
	return awaitInstanceTeardown(ctx, candidate)
}

func awaitInstanceTeardown(
	ctx context.Context,
	inst *DatabaseInstance,
) error {
	if inst == nil {
		return nil
	}
	done := startInstanceTeardown(inst)
	select {
	case <-done:
		inst.lifecycleMu.Lock()
		err := inst.lifecycleErr
		inst.lifecycleMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func startInstanceTeardown(inst *DatabaseInstance) <-chan struct{} {
	inst.lifecycleMu.Lock()
	defer inst.lifecycleMu.Unlock()
	if inst.lifecycleStarted {
		return inst.lifecycleDone
	}
	inst.lifecycleStarted = true
	inst.lifecycleDone = make(chan struct{})
	go runInstanceTeardown(inst, inst.lifecycleDone)
	return inst.lifecycleDone
}

func runInstanceTeardown(inst *DatabaseInstance, done chan struct{}) {
	release, drainErr := drainInstanceActions(inst)
	if inst.Cancel != nil {
		inst.Cancel()
	}
	release()
	timeout := inst.teardownTimeout
	if timeout <= 0 {
		timeout = defaultInstanceTeardownTimeout
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	workerErr := waitForInstanceWorkers(cleanupCtx, inst.Workers)
	shutdownErr := shutdownInstanceExecutor(cleanupCtx, inst)
	closeErr := closeInstancePool(cleanupCtx, inst)
	lifecycleErr := errors.Join(drainErr, workerErr, shutdownErr, closeErr)
	inst.lifecycleMu.Lock()
	inst.lifecycleErr = lifecycleErr
	close(done)
	inst.lifecycleMu.Unlock()
}

func waitForInstanceWorkers(
	ctx context.Context, workers *sync.WaitGroup,
) error {
	if workers == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for instance workers: %w", ctx.Err())
	}
}

func shutdownInstanceExecutor(
	ctx context.Context, inst *DatabaseInstance,
) error {
	done := make(chan error, 1)
	go func() {
		if inst.ExecutorShutdown != nil {
			done <- inst.ExecutorShutdown(ctx)
			return
		}
		if inst.Executor != nil {
			done <- inst.Executor.Shutdown(ctx)
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("shutting down instance executor: %w", ctx.Err())
	}
}

func closeInstancePool(
	ctx context.Context, inst *DatabaseInstance,
) error {
	done := make(chan struct{})
	go func() {
		if inst.ActionPool != nil {
			inst.ActionPool.Close()
		}
		if inst.PoolClose != nil {
			inst.PoolClose()
		} else if inst.Pool != nil {
			inst.Pool.Close()
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("closing instance pool: %w", ctx.Err())
	}
}

func (m *DatabaseManager) firstConnectedNameLocked() string {
	names := make([]string, 0, len(m.instances))
	for name, inst := range m.instances {
		if inst != nil && inst.Pool != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}
