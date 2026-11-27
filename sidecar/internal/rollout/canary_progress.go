package rollout

import (
	"context"
	"fmt"
	"slices"
	"strconv"
)

// progressBackend persists each target's part in a running canary as it
// happens (executor-04b), so a sidecar that stops mid-canary leaves the
// run, the target it was changing and that change's action on record.
// A progress write that fails, or that finds the run interrupted by
// another sidecar, fails the step: the engine then changes no further
// target and rolls back what this run changed.
type progressBackend struct {
	*canaryBackend
	runID string
}

// Reverify re-verifies the target, then records it as being changed (or
// as not locally verified) before the engine may change it.
func (p *progressBackend) Reverify(ctx context.Context, instance Instance,
	prior Prior) (Reverification, error) {
	candidate, err := p.canaryBackend.Reverify(ctx, instance, prior)
	if err != nil {
		return candidate, err
	}
	rec := InstanceRecord{InstanceID: instance.ID, Status: StatusApplying}
	if !candidate.Eligible {
		rec.Status, rec.Detail = StatusNotLocallyVerified, candidate.Reason
	}
	if err := p.save(ctx, rec); err != nil {
		return Reverification{}, err
	}
	return candidate, nil
}

// Measure records the change's action as awaiting its verdict, waits for
// the verdict, then records it.
func (p *progressBackend) Measure(ctx context.Context, instance Instance,
	change AppliedChange) (Outcome, error) {
	actionID, _ := strconv.ParseInt(change.Handle, 10, 64)
	if err := p.save(ctx, InstanceRecord{InstanceID: instance.ID,
		Status: StatusAwaitingVerdict, ActionLogID: actionID}); err != nil {
		return Outcome{}, err
	}
	outcome, err := p.canaryBackend.Measure(ctx, instance, change)
	if err != nil {
		return outcome, err
	}
	rec := InstanceRecord{InstanceID: instance.ID, Status: StatusApplied,
		ActionLogID: actionID, EvidenceID: outcome.EvidenceID,
		RegressionPct: outcome.RegressionPct, Detail: outcome.Detail,
		EffectPct: outcome.EffectPct, ControlPct: outcome.ControlPct,
		EffectMethod: outcome.EffectMethod}
	if outcome.Pending {
		rec.Status = StatusVerificationPending
	}
	if err := p.save(ctx, rec); err != nil {
		return Outcome{}, err
	}
	return outcome, nil
}

// save writes rec at the instance's place in the rollout order.
func (p *progressBackend) save(ctx context.Context, rec InstanceRecord) error {
	p.mu.Lock()
	rec.Ordinal = slices.Index(p.order, rec.InstanceID) + 1
	p.mu.Unlock()
	rec.UpdatedAt = p.svc.opts.Now().UTC()
	if err := p.svc.runs.SaveProgress(ctx, p.runID, rec); err != nil {
		return fmt.Errorf("stop the canary at %s: %w", rec.InstanceID, err)
	}
	return nil
}

// RecoverInterrupted marks every canary a stopped sidecar left running as
// interrupted, with the targets it was changing or measuring then. It is
// called once at startup and never resumes a run: a change already made
// stays with its database's own post-action monitor, and the operator
// decides what next. A run another live sidecar runs is left alone.
func (s *CanaryService) RecoverInterrupted(ctx context.Context) ([]InterruptedRun, error) {
	runs, err := s.runs.InterruptOrphaned(ctx, s.opts.Now().UTC())
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		s.opts.Log("rollout: canary %s from %s was interrupted by a sidecar restart "+
			"with %d database(s) in flight and %d keeping the change; it is not resumed",
			r.Run.EvidenceID, r.Run.SourceInstance, len(r.Changing), len(r.Kept))
	}
	return runs, nil
}
