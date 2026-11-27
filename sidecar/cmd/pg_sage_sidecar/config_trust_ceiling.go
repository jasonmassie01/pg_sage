package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// config-01: the trust ceilings (tier3 switches, maintenance window,
// rollback and cascade bounds) are live policy. The trust_policy owner
// installs them on every database's executor; a commit that fails part-way
// leaves each executor at the stricter of its old and the new ceiling.

type trustCeilingTarget struct {
	instance *fleet.DatabaseInstance
	old      executor.TrustCeiling
}

type preparedTrustCeiling struct {
	old, next executor.TrustCeiling
	targets   []trustCeilingTarget
	set       func(*fleet.DatabaseInstance, executor.TrustCeiling) error
	publish   func(executor.TrustCeiling)
	started   bool
	committed bool
}

// prepareCeilings plans the ceiling change for every running executor; it
// returns nil when the ceiling did not change.
func (o *trustPolicyOwner) prepareCeilings(
	manager *fleet.DatabaseManager, active, desired config.ConfigSnapshot,
) *preparedTrustCeiling {
	old := executor.TrustCeilingFromConfig(active.Config.Trust)
	next := executor.TrustCeilingFromConfig(desired.Config.Trust)
	if old == next {
		return nil
	}
	change := &preparedTrustCeiling{old: old, next: next, set: o.ceiling,
		publish: o.publish}
	if change.set == nil {
		change.set = applyTrustCeiling
	}
	for _, instance := range manager.Instances() {
		if instance.Executor == nil {
			continue // a failed placeholder has nothing to authorize
		}
		change.targets = append(change.targets, trustCeilingTarget{
			instance: instance, old: instance.Executor.TrustCeiling(),
		})
	}
	sort.Slice(change.targets, func(i, j int) bool {
		return change.targets[i].instance.Name < change.targets[j].instance.Name
	})
	return change
}

func (p *preparedTrustCeiling) commit() error {
	if p == nil {
		return nil
	}
	p.started = true
	for _, target := range p.targets {
		if err := p.set(target.instance, p.next); err != nil {
			return fmt.Errorf("db %q trust ceiling: %w", target.instance.Name, err)
		}
	}
	if p.publish != nil {
		p.publish(p.next)
	}
	p.committed = true
	return nil
}

// rollback runs after a failed commit (or a later owner's failure). Every
// target, reached or not, ends at the stricter of its old and the new
// ceiling, so neither a raise nor a half-applied downgrade survives as more
// autonomy. An uncommitted change (persistence failed) touches nothing.
func (p *preparedTrustCeiling) rollback() error {
	if p == nil || !p.started {
		return nil
	}
	var errs []string
	for _, target := range p.targets {
		stricter := executor.StricterTrustCeiling(target.old, p.next)
		if err := p.set(target.instance, stricter); err != nil {
			errs = append(errs, fmt.Sprintf("db %q: %v", target.instance.Name, err))
		}
	}
	if p.publish != nil {
		p.publish(executor.StricterTrustCeiling(p.old, p.next))
	}
	if len(errs) > 0 {
		return fmt.Errorf("restore trust ceiling: %s", strings.Join(errs, "; "))
	}
	return nil
}

// warnings names the tier3 switches a committed change turned on: live, but
// the trust ledger still gates every promotion beneath them.
func (p *preparedTrustCeiling) warnings() []string {
	if p == nil || !p.committed {
		return nil
	}
	var raised []string
	if !p.old.Tier3Safe && p.next.Tier3Safe {
		raised = append(raised, "trust.tier3_safe")
	}
	if !p.old.Tier3Moderate && p.next.Tier3Moderate {
		raised = append(raised, "trust.tier3_moderate")
	}
	if len(raised) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("trust ceiling raised (%s): takes effect on the next "+
		"cycle; the trust ledger still gates every promotion", strings.Join(raised, ", "))}
}

func applyTrustCeiling(instance *fleet.DatabaseInstance, ceiling executor.TrustCeiling) error {
	if instance.Executor == nil {
		return fmt.Errorf("executor is not running")
	}
	instance.Executor.SetTrustCeiling(&ceiling)
	return nil
}

// publishTrustCeiling writes the ceiling onto the process config so every
// executor built later in this process (a database added by reload, a meta
// reconnect) starts from it rather than from the startup values.
func publishTrustCeiling(ceiling executor.TrustCeiling) {
	if cfg == nil {
		return
	}
	config.LockForHotReload()
	defer config.UnlockForHotReload()
	ceiling.ApplyTo(&cfg.Trust)
}

// changedTrustCeiling is the desired ceiling when a config change moves it,
// else nil: an unchanged ceiling is already on the process config that new
// runtimes clone, including a stricter one a failed commit left behind.
func changedTrustCeiling(active, desired *config.Config) *executor.TrustCeiling {
	if active == nil || desired == nil {
		return nil
	}
	next := executor.TrustCeilingFromConfig(desired.Trust)
	if executor.TrustCeilingFromConfig(active.Trust) == next {
		return nil
	}
	return &next
}

// seedCeilings puts a reload's new trust ceiling on the runtimes it built in
// Prepare: their config clones predate the trust owner's publish, so they
// would keep the old ceiling for good. Until the reload commits, a candidate
// holds the stricter of its own and the new ceiling, so a raise that is not
// applied yet never runs.
func (p *preparedFleetReload) seedCeilings(committing bool) {
	if p.ceiling == nil {
		return
	}
	for _, candidate := range p.candidates {
		inst := candidate.instance
		if inst == nil || inst.Executor == nil {
			continue // a failed placeholder has nothing to authorize
		}
		ceiling := *p.ceiling
		if !committing {
			ceiling = executor.StricterTrustCeiling(inst.Executor.TrustCeiling(), ceiling)
		}
		inst.Executor.SetTrustCeiling(&ceiling)
	}
}
