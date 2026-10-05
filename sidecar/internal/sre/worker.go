package sre

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// stepMargin is the active time a sampling step must leave for its
// probes and the conclusion; with less, the worker evaluates what it has.
const stepMargin = 5 * time.Second

// Investigate runs one investigation to its conclusion if this worker can
// claim it. Work another worker holds, paused, finished or out-of-budget
// work returns nil; so does a lease lost mid-run (an operator stopped it
// or another worker took over): nothing more is committed.
func (c *Coordinator) Investigate(ctx context.Context, id UUID) error {
	scope, ok := c.Scope()
	if !ok {
		return fmt.Errorf("%w: coordinator is not bound", ErrInvalidRequest)
	}
	lease, err := c.store.Claim(ctx, scope, id, c.worker)
	switch {
	case errors.Is(err, ErrLeaseUnavailable), errors.Is(err, ErrTerminal),
		errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrBudgetExhausted):
		return nil
	case err != nil:
		c.durability.Observe(err)
		return err
	}
	err = c.runClaimed(ctx, lease)
	c.durability.Observe(err)
	if errors.Is(err, ErrLeaseLost) {
		c.logFn("INFO", "sre: investigation %s: lease lost; stopping", id)
		return nil
	}
	return err
}

func (c *Coordinator) runClaimed(ctx context.Context, lease Lease) error {
	inv, err := c.store.Get(ctx, lease.Scope, lease.InvestigationID)
	if err != nil {
		return err
	}
	plan, ok := c.plan(inv.TriggerKind)
	if !ok {
		return c.fail(ctx, lease, "no_probe_plan",
			fmt.Sprintf("no probe plan for trigger %q", inv.TriggerKind))
	}
	if lease, err = c.collect(ctx, lease, inv, plan); err != nil {
		return err
	}
	evidence, err := c.store.Evidence(ctx, lease.Scope, inv.ID)
	if err != nil {
		return err
	}
	obs, err := observations(evidence)
	if err != nil {
		return c.fail(ctx, lease, "unreadable_evidence", err.Error())
	}
	lease, d, evidence, run, err := c.applyRunbook(ctx, lease, inv, diagnose(inv, obs),
		evidence)
	if err != nil {
		return err
	}
	lease, d, model, err := c.consultModel(ctx, lease, inv, d, evidence)
	if err != nil {
		return err
	}
	return c.conclude(ctx, lease, d, model, attachments{runbook: run,
		proposals: c.advise(ctx, inv, d)})
}

// attachments are what a conclusion carries beside the diagnosis and
// the model output: the signed runbook that ran (M6 runbooks) and the
// custodian proposals of a runway diagnosis (M6 runways).
type attachments struct {
	runbook   *RunbookRun
	proposals []ActionProposal
}

// apply sets the attachments on a summary.
func (a attachments) apply(s *Summary) {
	s.Runbook, s.Proposals = a.runbook, a.proposals
}

// conclude persists the diagnosis with the runbook run, the custodian
// proposals and the model output beside it. If the store refuses the
// model output, the deterministic conclusion (with the runbook run, the
// proposals and the memory the model was offered) is persisted instead:
// the model never fails an investigation.
func (c *Coordinator) conclude(ctx context.Context, lease Lease, d causal.Diagnosis,
	model modelOutcome, extra attachments) error {
	conclusion := conclusionOf(d)
	extra.apply(&conclusion.Summary)
	model.apply(&conclusion.Summary)
	_, err := c.store.Conclude(ctx, lease, conclusion)
	if errors.Is(err, ErrInvalidRequest) && !model.empty() {
		c.logFn("WARN", "sre: investigation %s: model output refused at conclusion: %v",
			lease.InvestigationID, err)
		if rerr := c.store.RecordEvent(ctx, lease, EventModelRejected, map[string]any{
			"reason": RejectConclusion, "stage": stageVerify,
			"detail": truncateRunes(err.Error(), 300)}); rerr != nil {
			return rerr
		}
		if model.graph != nil { // the store refused an adopted model root
			d = *model.graph
		}
		fallback := conclusionOf(d)
		extra.apply(&fallback.Summary)
		fallback.Summary.Memory, fallback.Summary.Investigator = model.memory, model.run
		_, err = c.store.Conclude(ctx, lease, fallback)
	}
	if errors.Is(err, ErrInvalidRequest) {
		return c.fail(ctx, lease, "invalid_conclusion", err.Error())
	}
	return err
}

// collect runs the plan's remaining steps (a resumed investigation skips
// the steps already committed) and moves the investigation to evaluating.
func (c *Coordinator) collect(ctx context.Context, lease Lease, inv Investigation,
	plan []planStep) (Lease, error) {
	done, err := c.committedSteps(ctx, lease.Scope, inv.ID)
	if err != nil {
		return lease, err
	}
	state := inv.State
	for i, st := range plan {
		key := stepKey(i)
		if done[key] {
			continue
		}
		if st.sample {
			wait := c.cfg.SampleInterval * time.Duration(st.waits())
			if time.Until(lease.SegmentDeadline) < wait+stepMargin {
				break
			}
			if lease, err = c.waitHeld(ctx, lease, wait); err != nil {
				return lease, err
			}
		}
		if lease, err = c.store.Heartbeat(ctx, lease); err != nil {
			return lease, err
		}
		next := StateCollecting
		if i == len(plan)-1 {
			next = StateEvaluating
		}
		committed, err := c.store.CommitStep(ctx, lease, StepResult{IdempotencyKey: key,
			Results: c.runStep(ctx, st), NextState: next})
		if err != nil {
			return lease, err
		}
		state = committed.State
	}
	if state != StateEvaluating {
		// Keyed per claim: a resumed run (one that may already have moved
		// to evaluating and then needed more evidence) moves again.
		_, err = c.store.CommitStep(ctx, lease, StepResult{NextState: StateEvaluating,
			IdempotencyKey: fmt.Sprintf("evaluate-f%d", lease.Fence)})
	}
	return lease, err
}

// waitHeld waits between compared samples while heartbeating the lease:
// the wait may be as long as the lease TTL, so an unrenewed lease would
// expire under it. A lease lost meanwhile (an operator stop) ends the
// wait at the next heartbeat and is returned as the error.
func (c *Coordinator) waitHeld(ctx context.Context, lease Lease,
	d time.Duration) (Lease, error) {
	waitCtx, stop := c.keepAlive(ctx, lease)
	err := c.sleep(waitCtx, d)
	cur, hbErr := stop()
	switch {
	case hbErr != nil:
		return cur, hbErr
	case ctx.Err() != nil:
		return cur, ctx.Err()
	}
	return cur, err
}

func (c *Coordinator) committedSteps(ctx context.Context, scope Scope,
	id UUID) (map[string]bool, error) {
	evidence, err := c.store.Evidence(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	done := map[string]bool{}
	for _, e := range evidence {
		done[e.StepKey] = true
	}
	return done, nil
}

func stepKey(i int) string { return fmt.Sprintf("step-%d", i+1) }

func (c *Coordinator) runStep(ctx context.Context, st planStep) []probes.Result {
	out := make([]probes.Result, 0, len(st.calls))
	for _, call := range st.calls {
		out = append(out, c.runner.Run(ctx, call.id, call.args))
	}
	return out
}

// fail ends the run as failed with a code and a bounded reason.
func (c *Coordinator) fail(ctx context.Context, lease Lease, code, reason string) error {
	_, err := c.store.Conclude(ctx, lease, Conclusion{State: StateFailed,
		FailureCode: code, Summary: Summary{Reason: truncateRunes(reason, 500)}})
	return err
}
