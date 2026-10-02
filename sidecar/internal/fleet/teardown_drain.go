package fleet

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// defaultInstanceDrainTimeout bounds how long a removed or replaced runtime
// waits for in-flight actions before cancelling them. It covers ordinary
// DDL; a longer build is cancelled (its lease and slot are released by the
// executor's own cleanup) rather than holding a removal open.
const defaultInstanceDrainTimeout = 60 * time.Second

// drainInstanceActions runs the instance's Quiesce under its drain bound.
// The release it returns is never nil.
func drainInstanceActions(inst *DatabaseInstance) (func(), error) {
	if inst.Quiesce == nil {
		return func() {}, nil
	}
	timeout := inst.DrainTimeout
	if timeout <= 0 {
		timeout = defaultInstanceDrainTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	release, err := inst.Quiesce(ctx)
	if release == nil {
		release = func() {}
	}
	if err != nil {
		err = fmt.Errorf("draining db %q actions: %w", inst.Name, err)
	}
	return release, err
}

// UpdateMetadata swaps the configuration of the exact generation found by
// this mutation, for per-database settings that apply in place. The name
// is the instance's identity and never changes here.
func (op *LifecycleMutation) UpdateMetadata(
	expected *DatabaseInstance, cfg config.DatabaseConfig,
) error {
	if expected == nil || cfg.Name != expected.Name {
		return ErrInvalidInstance
	}
	m := op.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.instances[expected.Name] != expected {
		return ErrInstanceConflict
	}
	expected.Config = cfg
	return nil
}
