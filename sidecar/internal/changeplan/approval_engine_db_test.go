package changeplan

import (
	"testing"
	"time"
)

// plan-03a: the engine runs a step only when the plan's approval covers it
// or the step has its own approval at the current plan hash.

func (f *engineFixture) approvedAs(in Plan, mode Mode, upTo int) Plan {
	f.t.Helper()
	p := mustCreate(f.t, f.store, in)
	if _, err := f.store.ApproveWith(f.ctx, p.ID, decide3(p.PlanHash, mode, upTo), 7); err != nil {
		f.t.Fatalf("approve plan %d as %s: %v", p.ID, mode, err)
	}
	return f.get(p.ID)
}

// settle ticks a few times (a minute apart), judging judging steps.
func (f *engineFixture) settle(id int64, ticks int) Plan {
	f.t.Helper()
	for range ticks {
		f.judgeAll(id, "improved", "monitoring")
		f.tick()
		f.clock.Add(time.Minute)
	}
	return f.get(id)
}

func states(p Plan) []StepState {
	out := make([]StepState, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, s.Runtime.State)
	}
	return out
}

func TestModes_AllRunsEveryStep(t *testing.T) {
	f := newEngineFixture(t, "mode_all")
	p := f.approvedAs(chain(3, 101), ModeAll, 0)
	got := f.runUntil(p.ID, "improved", 20)
	if got.Status != StatusCompleted || f.manual.callCount() != 3 {
		t.Fatalf("plan %s with %d runs (%v), want completed with 3", got.Status,
			f.manual.callCount(), states(got))
	}
}

// up_to 2 runs steps 1 and 2 and stops before 3 with awaiting_approval;
// step 3's own approval lets it run, as its approver.
func TestModes_UpToTwoStopsBeforeThree(t *testing.T) {
	f := newEngineFixture(t, "mode_upto")
	p := f.approvedAs(chain(3, 101), ModeUpTo, 2)
	got := f.settle(p.ID, 14)
	st := states(got)
	if st[0] != StateKept || st[1] != StateKept || st[2] != StateAwaitingApproval ||
		got.Status != StatusRunning || f.manual.callCount() != 2 {
		t.Fatalf("plan %s steps %v, %d runs: want 1-2 kept, 3 awaiting_approval",
			got.Status, st, f.manual.callCount())
	}
	if r := got.Steps[2].Runtime.StateReason; r == "" {
		t.Fatal("step 3 waits with no reason")
	}
	if _, err := f.store.ApproveStep(f.ctx, p.ID, got.Steps[2].ID, got.PlanHash, 21,
		ViaCLI); err != nil {
		t.Fatalf("approve step 3: %v", err)
	}
	if s := f.get(p.ID).Steps[2].Runtime.State; s != StatePending {
		t.Fatalf("step 3 after its approval is %s, want pending", s)
	}
	done := f.runUntil(p.ID, "improved", 10)
	if done.Status != StatusCompleted || f.manual.callCount() != 3 {
		t.Fatalf("plan %s with %d runs, want completed with 3", done.Status,
			f.manual.callCount())
	}
	if c := f.manual.calls[2]; c.approver != 21 {
		t.Fatalf("step 3 ran as user %d, want its approver 21", c.approver)
	}
	if c := f.manual.calls[0]; c.approver != 7 {
		t.Fatalf("step 1 ran as user %d, want the plan's approver 7", c.approver)
	}
}

// step runs nothing until step 1 is approved, then only step 1.
func TestModes_StepRunsNothingUntilAStepIsApproved(t *testing.T) {
	f := newEngineFixture(t, "mode_step")
	p := f.approvedAs(chain(3, 101), ModeStep, 0)
	got := f.settle(p.ID, 4)
	if st := states(got); st[0] != StateAwaitingApproval || st[1] != StatePending ||
		f.manual.callCount() != 0 || len(f.queueItems(p.ID)) != 0 {
		t.Fatalf("steps %v, %d runs: want step 1 awaiting approval, nothing run", st,
			f.manual.callCount())
	}
	if _, err := f.store.ApproveStep(f.ctx, p.ID, got.Steps[0].ID, got.PlanHash, 7,
		ViaUI); err != nil {
		t.Fatalf("approve step 1: %v", err)
	}
	got = f.settle(p.ID, 8)
	if st := states(got); st[0] != StateKept || st[1] != StateAwaitingApproval ||
		st[2] != StatePending || f.manual.callCount() != 1 {
		t.Fatalf("steps %v, %d runs: want only step 1 run", st, f.manual.callCount())
	}
}

// Boundary: a never-automatic step at ordinal 1 under all waits for its
// own approval, then runs.
func TestModes_NeverAutomaticFirstStepUnderAllNeedsItsOwn(t *testing.T) {
	f := defaultCaps(t, "mode_own_first")
	in := restartPlan()
	in.Steps = in.Steps[1:]
	in.Steps[0].Ordinal = 1
	in.Edges = nil
	p := f.approvedAs(in, ModeAll, 0)
	got := f.settle(p.ID, 3)
	if got.Steps[0].Runtime.State != StateAwaitingApproval || f.manual.callCount() != 0 {
		t.Fatalf("step %+v, %d runs: want awaiting its own approval", got.Steps[0].Runtime,
			f.manual.callCount())
	}
	if _, err := f.store.ApproveStep(f.ctx, p.ID, got.Steps[0].ID, got.PlanHash, 8,
		ViaAPI); err != nil {
		t.Fatalf("approve step 1: %v", err)
	}
	got = f.settle(p.ID, 3)
	if f.manual.callCount() != 1 || got.Steps[0].Runtime.State == StateAwaitingApproval {
		t.Fatalf("step %+v, %d runs: want it run once its own approval is in",
			got.Steps[0].Runtime, f.manual.callCount())
	}
}
