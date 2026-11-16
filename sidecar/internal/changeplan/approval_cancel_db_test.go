package changeplan

import (
	"errors"
	"testing"
)

// plan-03a review: a plan stopped before a step awaiting its own approval
// is running with nothing in flight; a person declines that step by
// cancelling the plan, and nothing of it runs after.

// No concurrent access test here: Cancel and the engine's tick both lock
// the plan row, which TestApproveWhileEditing_* already exercises.

func TestCancel_RunningPlanAwaitingAStepApproval(t *testing.T) {
	f := newEngineFixture(t, "cancel_awaiting")
	p := f.approvedAs(chain(2, 101), ModeUpTo, 1)
	got := f.settle(p.ID, 10)
	if st := states(got); got.Status != StatusRunning || st[0] != StateKept ||
		st[1] != StateAwaitingApproval || f.manual.callCount() != 1 {
		t.Fatalf("plan %s steps %v, %d runs: want running, step 2 awaiting approval",
			got.Status, st, f.manual.callCount())
	}
	if _, err := f.store.ApproveStep(f.ctx, p.ID, got.Steps[1].ID, "", 9,
		ViaAPI); decisionCode(err) != CodeHashRequired {
		t.Fatalf("approve step 2 without a hash: %v", err)
	}
	if err := f.store.Cancel(f.ctx, p.ID, "not step 2", 9); err != nil {
		t.Fatalf("cancel a plan awaiting a step approval: %v", err)
	}
	after := f.get(p.ID)
	if after.Status != StatusCancelled {
		t.Fatalf("status %s, want cancelled", after.Status)
	}
	if err := f.store.Verify(f.ctx, p.ID); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("the cancelled plan's approval: %v, want voided", err)
	}
	ev := f.events(p.ID)
	last := ev[len(ev)-1]
	if last.Kind != EventCancelled || last.Actor != "user:9" ||
		last.Detail["from_status"] != string(StatusRunning) ||
		last.Detail["reason"] != "not step 2" {
		t.Fatalf("cancelled event %+v, want from running by user:9 with the reason", last)
	}
	f.settle(p.ID, 4)
	if f.manual.callCount() != 1 {
		t.Fatalf("%d runs after the cancel, want still 1", f.manual.callCount())
	}
	if _, err := f.store.ApproveStep(f.ctx, p.ID, got.Steps[1].ID, got.PlanHash, 9,
		ViaAPI); !errors.Is(err, ErrNotApprovable) {
		t.Fatalf("approve a step of a cancelled plan: %v, want ErrNotApprovable", err)
	}
}

// cancellable refuses a running plan while any step is in flight, and
// every status but approved, halted and running.
func TestCancellable_EachStatusAndStepState(t *testing.T) {
	plan := func(status Status, states ...StepState) Plan {
		p := Plan{Status: status}
		for i, st := range states {
			p.Steps = append(p.Steps, Step{Ordinal: i + 1,
				Runtime: StepRuntime{State: st}})
		}
		return p
	}
	for st := range inFlightStates {
		if cancellable(plan(StatusRunning, StateKept, st, StatePending)) {
			t.Errorf("running with a step %s is cancellable", st)
		}
	}
	yes := map[string]Plan{
		"approved":                  plan(StatusApproved, StatePending),
		"halted":                    plan(StatusHalted, StateFailed, StatePending),
		"running awaiting approval": plan(StatusRunning, StateKept, StateAwaitingApproval),
		"running awaiting operator": plan(StatusRunning, StateAwaitingOperator),
		"running between steps":     plan(StatusRunning, StateKept, StatePending),
		"running, no steps":         plan(StatusRunning),
	}
	for name, p := range yes {
		if !cancellable(p) {
			t.Errorf("%s is not cancellable", name)
		}
	}
	for _, st := range []Status{StatusDraft, StatusProposed, StatusCompleted,
		StatusReverted, StatusRejected, StatusVoided, StatusCancelled} {
		if cancellable(plan(st, StatePending)) {
			t.Errorf("a %s plan is cancellable", st)
		}
	}
}
