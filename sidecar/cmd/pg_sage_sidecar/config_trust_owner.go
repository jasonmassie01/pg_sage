package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

type trustPolicyOwner struct {
	manager func() *fleet.DatabaseManager
	apply   func(*fleet.DatabaseInstance, string) error
}

func (o *trustPolicyOwner) Name() string { return "trust_policy" }

func (o *trustPolicyOwner) Prepare(
	_ context.Context, active, desired config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	change := &preparedTrustPolicy{level: desired.Config.Trust.Level}
	change.apply = o.apply
	if change.apply == nil {
		change.apply = applyTrustPolicyLevel
	}
	manager := o.manager()
	if manager == nil {
		return change, nil
	}
	for _, instance := range manager.Instances() {
		if instance.HasTrustLevelOverride() {
			continue
		}
		oldLevel := active.Config.Trust.Level
		if instance.Executor != nil {
			oldLevel = instance.Executor.TrustLevel()
		} else if status := instance.SnapshotStatus(); status.TrustLevel != "" {
			oldLevel = status.TrustLevel
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

type trustPolicyTarget struct {
	instance *fleet.DatabaseInstance
	oldLevel string
}

type preparedTrustPolicy struct {
	level     string
	targets   []trustPolicyTarget
	committed int
	apply     func(*fleet.DatabaseInstance, string) error
}

func (p *preparedTrustPolicy) Commit(context.Context) error {
	for i, target := range p.targets {
		if err := p.apply(target.instance, p.level); err != nil {
			return fmt.Errorf("db %q trust policy: %w",
				target.instance.Name, err)
		}
		p.committed = i + 1
	}
	return nil
}

func (p *preparedTrustPolicy) Rollback(context.Context) error {
	var rollbackErr error
	for i := p.committed - 1; i >= 0; i-- {
		target := p.targets[i]
		if err := p.apply(target.instance, target.oldLevel); err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
		}
	}
	return rollbackErr
}

func (*preparedTrustPolicy) Drain(context.Context) error { return nil }

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
