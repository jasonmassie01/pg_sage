package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Commit validates that every observed generation is still current, then
// publishes the whole plan in one lifecycle reservation.
func (p *preparedFleetReload) Commit(ctx context.Context) error {
	steps := p.steps()
	err := p.mgr.WithLifecycle(ctx, func(op *fleet.LifecycleMutation) error {
		if err := p.validate(op); err != nil {
			return err
		}
		return runLifecycleSteps(op, steps)
	})
	if err != nil {
		return fmt.Errorf("publish fleet reload: %w", err)
	}
	p.committed = true
	p.next.publish()
	for _, candidate := range p.candidates {
		if candidate.instance.Pool != nil {
			updateInstanceFindings(context.Background(), candidate.instance)
		}
	}
	return nil
}

// Rollback restores the previous generations after a commit (another
// owner failed) and stops every candidate runtime.
func (p *preparedFleetReload) Rollback(ctx context.Context) error {
	var rollbackErr error
	if p.committed {
		steps := p.steps()
		rollbackErr = p.mgr.WithLifecycle(context.WithoutCancel(ctx),
			func(op *fleet.LifecycleMutation) error {
				return undoLifecycleSteps(op, steps, len(steps))
			})
		p.committed = false
		p.previous.publish()
	}
	return errors.Join(rollbackErr, p.discardCandidates())
}

// Drain retires every removed or replaced generation: in-flight actions
// finish (bounded), then workers, executor and pool stop. A removed
// database's budget share returns to the rest of the fleet.
func (p *preparedFleetReload) Drain(ctx context.Context) error {
	retired := append([]*fleet.DatabaseInstance(nil), p.removals...)
	for _, candidate := range p.candidates {
		if candidate.old != nil {
			retired = append(retired, candidate.old)
		}
	}
	err := shutdownInstances(ctx, retired)
	for _, inst := range p.removals {
		// A later reload may already have added the name back.
		if p.mgr.GetInstance(inst.Name) == nil {
			unregisterFleetBudget(inst.Name)
		}
		logInfo("fleet", "db %q: removed by reload", inst.Name)
	}
	return err
}

// discardCandidates stops prepared runtimes that were never published.
func (p *preparedFleetReload) discardCandidates() error {
	var instances []*fleet.DatabaseInstance
	for _, candidate := range p.candidates {
		if candidate.instance == nil {
			continue
		}
		instances = append(instances, candidate.instance)
		if candidate.old == nil && p.mgr.GetInstance(candidate.cfg.Name) == nil {
			unregisterFleetBudget(candidate.cfg.Name)
		}
	}
	return shutdownInstances(context.Background(), instances)
}

func shutdownInstances(ctx context.Context, instances []*fleet.DatabaseInstance) error {
	errs := make([]error, len(instances))
	var wg sync.WaitGroup
	for i, inst := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fleet.ShutdownInstance(ctx, inst); err != nil {
				errs[i] = fmt.Errorf("db %q: %w", inst.Name, err)
				logWarn("fleet", "db %q: runtime drain incomplete: %v", inst.Name, err)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// validate checks every observed generation is still the published one.
func (p *preparedFleetReload) validate(op *fleet.LifecycleMutation) error {
	for _, candidate := range p.candidates {
		name := candidate.cfg.Name
		if candidate.old == nil {
			if err := op.ValidateRegistration(name); err != nil {
				return err
			}
			continue
		}
		if _, err := op.ValidateReplacement(name, name, candidate.old); err != nil {
			return fmt.Errorf("db %q: %w", name, err)
		}
	}
	for _, inst := range p.removals {
		if _, err := op.ValidateReplacement(inst.Name, inst.Name, inst); err != nil {
			return fmt.Errorf("db %q: %w", inst.Name, err)
		}
	}
	for _, change := range p.hot {
		name := change.instance.Name
		if _, err := op.ValidateReplacement(name, name, change.instance); err != nil {
			return fmt.Errorf("db %q: %w", name, err)
		}
	}
	return nil
}

// lifecycleStep is one reversible publication.
type lifecycleStep struct {
	do, undo func(*fleet.LifecycleMutation) error
}

func runLifecycleSteps(op *fleet.LifecycleMutation, steps []lifecycleStep) error {
	for i, step := range steps {
		if err := step.do(op); err != nil {
			return errors.Join(err, undoLifecycleSteps(op, steps, i))
		}
	}
	return nil
}

// undoLifecycleSteps reverses the first done steps, newest first.
func undoLifecycleSteps(op *fleet.LifecycleMutation, steps []lifecycleStep, done int) error {
	var undoErr error
	for i := done - 1; i >= 0; i-- {
		undoErr = errors.Join(undoErr, steps[i].undo(op))
	}
	return undoErr
}

func (p *preparedFleetReload) steps() []lifecycleStep {
	steps := make([]lifecycleStep, 0, len(p.removals)+len(p.candidates)+len(p.hot))
	for _, inst := range p.removals {
		steps = append(steps, lifecycleStep{
			do:   func(op *fleet.LifecycleMutation) error { return op.DetachInstance(inst) },
			undo: func(op *fleet.LifecycleMutation) error { return op.PublishRegistration(inst) },
		})
	}
	for _, candidate := range p.candidates {
		steps = append(steps, candidateStep(candidate))
	}
	for _, change := range p.hot {
		steps = append(steps, lifecycleStep{
			do: func(op *fleet.LifecycleMutation) error {
				return applyFleetDatabasePolicy(op, change.instance, change.old, change.cfg)
			},
			undo: func(op *fleet.LifecycleMutation) error {
				return applyFleetDatabasePolicy(op, change.instance, change.cfg, change.old)
			},
		})
	}
	return steps
}

func candidateStep(candidate fleetCandidate) lifecycleStep {
	name, old, inst := candidate.cfg.Name, candidate.old, candidate.instance
	if old == nil {
		return lifecycleStep{
			do:   func(op *fleet.LifecycleMutation) error { return op.PublishRegistration(inst) },
			undo: func(op *fleet.LifecycleMutation) error { return op.DetachInstance(inst) },
		}
	}
	return lifecycleStep{
		do: func(op *fleet.LifecycleMutation) error {
			return op.PublishReplacement(name, old, inst)
		},
		undo: func(op *fleet.LifecycleMutation) error {
			return op.PublishReplacement(name, inst, old)
		},
	}
}

// applyFleetDatabasePolicy moves a running database from one config to
// another in place: instance metadata, executor trust, mode and gate. A
// raise is logged; earned autonomy (M7) still gates incident families,
// and no carried-over autonomy is seeded for it as a restart would.
func applyFleetDatabasePolicy(
	op *fleet.LifecycleMutation, inst *fleet.DatabaseInstance,
	from, to config.DatabaseConfig,
) error {
	if err := op.UpdateMetadata(inst, to); err != nil {
		return err
	}
	level := to.TrustLevel
	if inst.Executor != nil {
		if err := inst.Executor.SetTrustLevel(to.TrustLevel); err != nil {
			return fmt.Errorf("db %q: %w", inst.Name, err)
		}
		inst.Executor.SetExecutionMode(resolveStaticFleetExecMode(to))
		inst.Executor.SetExecutorEnabled(to.IsExecutorEnabled())
		level = inst.Executor.TrustLevel()
	}
	inst.SetTrustLevelOverride(to.TrustLevelExplicit)
	inst.UpdateStatus(func(status *fleet.InstanceStatus) { status.TrustLevel = level })
	if trustRank(to.TrustLevel) > trustRank(from.TrustLevel) {
		logWarn("fleet", "db %q: trust raised %s -> %s by reload", inst.Name,
			from.TrustLevel, to.TrustLevel)
	}
	logInfo("fleet", "db %q: applied in place: %v", inst.Name,
		config.ClassifyDatabaseChange(from, to).Fields)
	return nil
}
