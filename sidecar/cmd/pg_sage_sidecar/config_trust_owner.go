package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

type trustPolicyOwner struct {
	manager func() *fleet.DatabaseManager
	apply   func(*fleet.DatabaseInstance, string) error
	// persist durably writes one database's trust row. It is set only in
	// meta-db mode, where every database owns an explicit trust policy and
	// the global level acts as a ceiling (G5-B14).
	persist func(context.Context, *fleet.DatabaseInstance, string) error
}

// newTrustPolicyOwner builds the process trust owner; in meta-db mode it
// persists global downgrades into each database's trust row.
func newTrustPolicyOwner() *trustPolicyOwner {
	owner := &trustPolicyOwner{manager: func() *fleet.DatabaseManager {
		return fleetMgr
	}}
	if globalMetaState != nil && globalMetaState.Pool != nil {
		owner.persist = persistMetaTrustLevel
	}
	return owner
}

// persistMetaTrustLevel writes a database's trust row in the meta DB so a
// global downgrade survives restart and reconnect (buildExecutor reads it).
func persistMetaTrustLevel(
	ctx context.Context, instance *fleet.DatabaseInstance, level string,
) error {
	if globalMetaState == nil || globalMetaState.Pool == nil {
		return fmt.Errorf("meta database is unavailable")
	}
	if instance.DatabaseID <= 0 {
		return fmt.Errorf("db %q has no meta database id", instance.Name)
	}
	_, err := store.NewConfigStore(globalMetaState.Pool).SetDatabaseTrustPolicy(
		ctx, instance.DatabaseID, level,
	)
	return err
}

func (o *trustPolicyOwner) Name() string { return "trust_policy" }

func (o *trustPolicyOwner) Prepare(
	_ context.Context, active, desired config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	change := &preparedTrustPolicy{
		level: desired.Config.Trust.Level, apply: o.apply, persist: o.persist,
	}
	if change.apply == nil {
		change.apply = applyTrustPolicyLevel
	}
	manager := o.manager()
	if manager == nil {
		return change, nil
	}
	for _, instance := range manager.Instances() {
		oldLevel := currentTrustLevel(instance, active.Config.Trust.Level)
		if o.persist != nil {
			change.addCeilingTarget(instance, oldLevel)
			continue
		}
		if instance.HasTrustLevelOverride() {
			continue
		}
		change.targets = append(change.targets, trustPolicyTarget{
			instance: instance, oldLevel: oldLevel,
		})
	}
	sort.Slice(change.targets, func(i, j int) bool {
		return change.targets[i].instance.Name < change.targets[j].instance.Name
	})
	return change, nil
}

func currentTrustLevel(instance *fleet.DatabaseInstance, global string) string {
	if instance.Executor != nil {
		return instance.Executor.TrustLevel()
	}
	if status := instance.SnapshotStatus(); status.TrustLevel != "" {
		return status.TrustLevel
	}
	if instance.Config.TrustLevel != "" {
		return instance.Config.TrustLevel
	}
	return global
}

type trustPolicyTarget struct {
	instance *fleet.DatabaseInstance
	oldLevel string
}

type preparedTrustPolicy struct {
	level     string
	targets   []trustPolicyTarget
	committed int
	notRaised int
	apply     func(*fleet.DatabaseInstance, string) error
	persist   func(context.Context, *fleet.DatabaseInstance, string) error
}

// addCeilingTarget lowers databases above the new global level and never
// raises one: a raise would escalate past a database's own policy.
func (p *preparedTrustPolicy) addCeilingTarget(
	instance *fleet.DatabaseInstance, oldLevel string,
) {
	switch {
	case trustRank(oldLevel) > trustRank(p.level):
		p.targets = append(p.targets, trustPolicyTarget{
			instance: instance, oldLevel: oldLevel,
		})
	case trustRank(oldLevel) < trustRank(p.level):
		p.notRaised++
	}
}

func (p *preparedTrustPolicy) Commit(ctx context.Context) error {
	for i, target := range p.targets {
		if err := p.set(ctx, target, p.level); err != nil {
			return fmt.Errorf("db %q trust policy: %w",
				target.instance.Name, err)
		}
		p.committed = i + 1
	}
	return nil
}

// set applies a level in memory and, in meta mode, durably; a durable
// failure restores the in-memory level so the two never diverge.
func (p *preparedTrustPolicy) set(
	ctx context.Context, target trustPolicyTarget, level string,
) error {
	if err := p.apply(target.instance, level); err != nil {
		return err
	}
	if p.persist == nil {
		return nil
	}
	if err := p.persist(ctx, target.instance, level); err != nil {
		return errors.Join(err, p.apply(target.instance, target.oldLevel))
	}
	return nil
}

func (p *preparedTrustPolicy) Rollback(ctx context.Context) error {
	var rollbackErr error
	for i := p.committed - 1; i >= 0; i-- {
		target := p.targets[i]
		restore := trustPolicyTarget{instance: target.instance, oldLevel: p.level}
		if err := p.set(ctx, restore, target.oldLevel); err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
		}
	}
	return rollbackErr
}

func (*preparedTrustPolicy) Drain(context.Context) error { return nil }

// Warnings reports meta-db databases a global raise left at their own,
// lower trust policy, so the API does not imply they were escalated.
func (p *preparedTrustPolicy) Warnings() []string {
	if p.notRaised == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"trust.level %s is a ceiling in meta-db mode: %d databases were "+
			"not raised; raise each database's trust level individually",
		p.level, p.notRaised)}
}

func trustRank(level string) int {
	switch level {
	case "observation":
		return 0
	case "advisory":
		return 1
	case "autonomous":
		return 2
	}
	return -1
}

func applyTrustPolicyLevel(
	instance *fleet.DatabaseInstance, level string,
) error {
	if instance.Executor != nil {
		if err := instance.Executor.SetTrustLevel(level); err != nil {
			return err
		}
	}
	instance.UpdateStatus(func(status *fleet.InstanceStatus) {
		status.TrustLevel = level
	})
	return nil
}
