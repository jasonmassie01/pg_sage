package config

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

var ErrGenerationConflict = errors.New("config generation conflict")

// ConfigSnapshot is an immutable, versioned effective configuration.
type ConfigSnapshot struct {
	Generation uint64
	Config     *Config
}

// ApplyResult distinguishes persisted desired state from acknowledged state.
type ApplyResult struct {
	DesiredGeneration uint64            `json:"desired_generation"`
	ActiveGeneration  uint64            `json:"active_generation"`
	Applied           []string          `json:"applied"`
	PendingRestart    []string          `json:"pending_restart"`
	ComponentStatus   map[string]string `json:"component_status"`
	Warnings          []string          `json:"warnings,omitempty"`
}

// RevisionStore atomically persists a complete desired revision.
type RevisionStore interface {
	PersistDesired(context.Context, ConfigSnapshot) error
}

// PersistDesiredFunc persists a request-scoped desired revision atomically.
type PersistDesiredFunc func(context.Context, ConfigSnapshot) error

// ReconfigurationOwner prepares a resource-owning component for a new config.
type ReconfigurationOwner interface {
	Name() string
	Prepare(context.Context, ConfigSnapshot, ConfigSnapshot) (
		PreparedReconfiguration, error)
}

// PreparedReconfiguration is unpublished until Commit acknowledges the swap.
type PreparedReconfiguration interface {
	Commit(context.Context) error
	Rollback(context.Context) error
	Drain(context.Context) error
}

// ConfigController serializes writers and atomically publishes reader snapshots.
type ConfigController struct {
	applyMu    sync.Mutex
	active     atomic.Pointer[ConfigSnapshot]
	desired    atomic.Pointer[ConfigSnapshot]
	store      RevisionStore
	owners     map[string]ReconfigurationOwner
	ownerOrder []string
}

// RegisterOwner adds a runtime resource owner before configuration writers
// begin. Registration is serialized with Apply and never replaces an owner.
func (c *ConfigController) RegisterOwner(owner ReconfigurationOwner) error {
	if owner == nil || owner.Name() == "" {
		return fmt.Errorf("config owner is nil or unnamed")
	}
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	name := owner.Name()
	if _, exists := c.owners[name]; exists {
		return fmt.Errorf("config owner %q is already registered", name)
	}
	c.owners[name] = owner
	c.ownerOrder = append(c.ownerOrder, name)
	return nil
}

// NewConfigController publishes generation one from a detached initial config.
func NewConfigController(
	initial *Config, store RevisionStore, owners ...ReconfigurationOwner,
) *ConfigController {
	return NewConfigControllerAtGeneration(initial, 1, store, owners...)
}

// NewConfigControllerAtGeneration restores the last durable generation.
func NewConfigControllerAtGeneration(
	initial *Config, generation uint64, store RevisionStore,
	owners ...ReconfigurationOwner,
) *ConfigController {
	if initial == nil {
		initial = DefaultConfig()
	}
	if generation == 0 {
		generation = 1
	}
	controller := &ConfigController{
		store:  store,
		owners: make(map[string]ReconfigurationOwner, len(owners)),
	}
	for _, owner := range owners {
		if owner != nil && owner.Name() != "" {
			if _, exists := controller.owners[owner.Name()]; !exists {
				controller.ownerOrder = append(controller.ownerOrder, owner.Name())
			}
			controller.owners[owner.Name()] = owner
		}
	}
	active := &ConfigSnapshot{Generation: generation, Config: Clone(initial)}
	desired := &ConfigSnapshot{Generation: generation, Config: Clone(initial)}
	controller.active.Store(active)
	controller.desired.Store(desired)
	return controller
}

// Active returns a detached copy of the last fully acknowledged snapshot.
func (c *ConfigController) Active() ConfigSnapshot {
	return cloneSnapshot(c.active.Load())
}

// Desired returns a detached copy of the last persisted desired snapshot.
func (c *ConfigController) Desired() ConfigSnapshot {
	return cloneSnapshot(c.desired.Load())
}

// PendingRestart returns desired fields that are not active yet.
func (c *ConfigController) PendingRestart() []string {
	active := c.Active()
	desired := c.Desired()
	changed := changedConfigPaths(active.Config, desired.Config)
	result, _ := c.planApply(
		active.Generation, desired.Generation, changed,
	)
	return append([]string(nil), result.PendingRestart...)
}

// EffectiveLifecycle fails closed to restart when a reconfigure owner is absent.
func (c *ConfigController) EffectiveLifecycle(path string) (ConfigLifecycle, bool) {
	metadata, ok := LookupFieldLifecycle(path)
	if !ok {
		return "", false
	}
	if metadata.Owner != "" && c.owners[metadata.Owner] == nil &&
		(metadata.Lifecycle == LifecycleReconfigure ||
			metadata.Lifecycle == LifecycleLivePolicy) {
		return LifecycleRestart, true
	}
	return metadata.Lifecycle, true
}

// Apply validates, persists, acknowledges, and publishes one complete candidate.
func (c *ConfigController) Apply(
	ctx context.Context, expectedGeneration uint64, candidate *Config,
) (ApplyResult, error) {
	return c.apply(ctx, expectedGeneration, candidate, nil)
}

// ApplyWithPersistence serializes request-scoped persistence with CAS and
// publication. A persistence failure leaves both controller snapshots intact.
func (c *ConfigController) ApplyWithPersistence(
	ctx context.Context, expectedGeneration uint64, candidate *Config,
	persist PersistDesiredFunc,
) (ApplyResult, error) {
	if persist == nil {
		return ApplyResult{}, fmt.Errorf("persist desired function is nil")
	}
	return c.apply(ctx, expectedGeneration, candidate, persist)
}

func (c *ConfigController) apply(
	ctx context.Context, expectedGeneration uint64, candidate *Config,
	persist PersistDesiredFunc,
) (ApplyResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if candidate == nil {
		return ApplyResult{}, fmt.Errorf("candidate config is nil")
	}
	candidate = Clone(candidate)
	if err := candidate.validate(); err != nil {
		return ApplyResult{}, fmt.Errorf("validate candidate: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ApplyResult{}, err
	}

	c.applyMu.Lock()
	active := c.Active()
	desired := c.Desired()
	if desired.Generation != expectedGeneration {
		c.applyMu.Unlock()
		return ApplyResult{}, fmt.Errorf(
			"%w: expected %d, desired %d",
			ErrGenerationConflict, expectedGeneration, desired.Generation,
		)
	}
	if len(changedConfigPaths(desired.Config, candidate)) == 0 {
		if persist != nil ||
			len(changedConfigPaths(active.Config, candidate)) > 0 {
			return c.applyLocked(ctx, active, candidate, persist)
		}
		c.applyMu.Unlock()
		return unchangedResult(
			desired.Generation, active.Generation,
		), nil
	}
	return c.applyLocked(ctx, active, candidate, persist)
}

func (c *ConfigController) applyLocked(
	ctx context.Context, active ConfigSnapshot, candidate *Config,
	persist PersistDesiredFunc,
) (ApplyResult, error) {
	changed := changedConfigPaths(active.Config, candidate)
	if len(changed) == 0 {
		desiredGeneration := nextGeneration(
			active.Generation, c.desired.Load().Generation,
		)
		revision := ConfigSnapshot{
			Generation: desiredGeneration, Config: Clone(candidate),
		}
		if err := c.persistDesired(ctx, revision, persist); err != nil {
			c.applyMu.Unlock()
			return ApplyResult{}, err
		}
		c.active.Store(snapshotPointer(revision))
		c.applyMu.Unlock()
		return unchangedResult(desiredGeneration, desiredGeneration), nil
	}
	desiredGeneration := nextGeneration(active.Generation, c.desired.Load().Generation)
	desired := ConfigSnapshot{Generation: desiredGeneration, Config: Clone(candidate)}
	result, ownerNames := c.planApply(active.Generation, desiredGeneration, changed)
	if len(result.Applied) == 0 {
		return c.persistPending(ctx, desired, result, persist)
	}
	effective := ConfigSnapshot{
		Generation: desiredGeneration,
		Config:     configWithPaths(active.Config, candidate, result.Applied),
	}

	prepared, err := c.prepareOwners(ctx, ownerNames, active, effective)
	if err != nil {
		c.applyMu.Unlock()
		return ApplyResult{}, err
	}
	if err := c.persistDesired(ctx, desired, persist); err != nil {
		rollbackPrepared(ctx, prepared)
		c.applyMu.Unlock()
		return ApplyResult{}, err
	}
	if err := commitPrepared(ctx, prepared); err != nil {
		rollbackPrepared(ctx, prepared)
		result.PendingRestart = append(
			result.PendingRestart, result.Applied...,
		)
		result.Applied = nil
		result.Warnings = append(result.Warnings, err.Error())
		for _, name := range ownerNames {
			result.ComponentStatus[name] = "commit_failed"
		}
		c.applyMu.Unlock()
		return result, nil
	}
	c.active.Store(snapshotPointer(effective))
	result.ActiveGeneration = desiredGeneration
	for _, name := range ownerNames {
		result.ComponentStatus[name] = "applied"
	}
	c.applyMu.Unlock()
	if err := drainPrepared(ctx, prepared); err != nil {
		result.Warnings = append(result.Warnings, err.Error())
	}
	return result, nil
}

func (c *ConfigController) planApply(
	activeGeneration, desiredGeneration uint64, changed []string,
) (ApplyResult, []string) {
	result := ApplyResult{
		DesiredGeneration: desiredGeneration,
		ActiveGeneration:  activeGeneration,
		ComponentStatus:   make(map[string]string),
	}
	ownerSet := make(map[string]struct{})
	for _, path := range changed {
		lifecycle, _ := c.EffectiveLifecycle(path)
		if lifecycle == LifecycleRestart || lifecycle == LifecycleAPI {
			result.PendingRestart = append(result.PendingRestart, path)
			continue
		}
		result.Applied = append(result.Applied, path)
		metadata, _ := LookupFieldLifecycle(path)
		if lifecycle == LifecycleReconfigure ||
			(lifecycle == LifecycleLivePolicy && metadata.Owner != "" &&
				c.owners[metadata.Owner] != nil) {
			ownerSet[metadata.Owner] = struct{}{}
		}
	}
	ownerNames := make([]string, 0, len(ownerSet))
	for _, name := range c.ownerOrder {
		if _, ok := ownerSet[name]; ok {
			ownerNames = append(ownerNames, name)
		}
	}
	return result, ownerNames
}

func (c *ConfigController) persistPending(
	ctx context.Context, desired ConfigSnapshot, result ApplyResult,
	persist PersistDesiredFunc,
) (ApplyResult, error) {
	result.Applied = nil
	if err := c.persistDesired(ctx, desired, persist); err != nil {
		c.applyMu.Unlock()
		return ApplyResult{}, err
	}
	c.applyMu.Unlock()
	return result, nil
}

func (c *ConfigController) prepareOwners(
	ctx context.Context, names []string, active, desired ConfigSnapshot,
) ([]preparedOwner, error) {
	prepared := make([]preparedOwner, 0, len(names))
	for _, name := range names {
		change, err := c.owners[name].Prepare(
			ctx, cloneSnapshotValue(active), cloneSnapshotValue(desired))
		if err != nil {
			rollbackPrepared(ctx, prepared)
			return nil, fmt.Errorf("prepare config owner %q: %w", name, err)
		}
		if change == nil {
			rollbackPrepared(ctx, prepared)
			return nil, fmt.Errorf("prepare config owner %q returned nil", name)
		}
		prepared = append(prepared, preparedOwner{name: name, change: change})
	}
	return prepared, nil
}

func (c *ConfigController) persistDesired(
	ctx context.Context, desired ConfigSnapshot, persist PersistDesiredFunc,
) error {
	if persist != nil {
		if err := persist(ctx, cloneSnapshotValue(desired)); err != nil {
			return fmt.Errorf(
				"persist desired config generation %d: %w",
				desired.Generation, err,
			)
		}
	} else if c.store != nil {
		if err := c.store.PersistDesired(ctx, cloneSnapshotValue(desired)); err != nil {
			return fmt.Errorf("persist desired config generation %d: %w", desired.Generation, err)
		}
	}
	c.desired.Store(snapshotPointer(desired))
	return nil
}

type preparedOwner struct {
	name   string
	change PreparedReconfiguration
}

func commitPrepared(ctx context.Context, prepared []preparedOwner) error {
	for _, owner := range prepared {
		if err := owner.change.Commit(ctx); err != nil {
			return fmt.Errorf("commit config owner %q: %w", owner.name, err)
		}
	}
	return nil
}

func rollbackPrepared(ctx context.Context, prepared []preparedOwner) {
	for i := len(prepared) - 1; i >= 0; i-- {
		_ = prepared[i].change.Rollback(ctx)
	}
}

func drainPrepared(ctx context.Context, prepared []preparedOwner) error {
	for _, owner := range prepared {
		if err := owner.change.Drain(ctx); err != nil {
			return fmt.Errorf("drain config owner %q: %w", owner.name, err)
		}
	}
	return nil
}

func cloneSnapshot(snapshot *ConfigSnapshot) ConfigSnapshot {
	if snapshot == nil {
		return ConfigSnapshot{}
	}
	return cloneSnapshotValue(*snapshot)
}

func cloneSnapshotValue(snapshot ConfigSnapshot) ConfigSnapshot {
	return ConfigSnapshot{Generation: snapshot.Generation, Config: Clone(snapshot.Config)}
}

func snapshotPointer(snapshot ConfigSnapshot) *ConfigSnapshot {
	cloned := cloneSnapshotValue(snapshot)
	return &cloned
}

func nextGeneration(left, right uint64) uint64 {
	if right > left {
		return right + 1
	}
	return left + 1
}

func unchangedResult(desiredGeneration, activeGeneration uint64) ApplyResult {
	return ApplyResult{
		DesiredGeneration: desiredGeneration,
		ActiveGeneration:  activeGeneration,
		ComponentStatus:   make(map[string]string),
	}
}
