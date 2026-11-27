package changeplan

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// plan-02c: the revert_to_checkpoint walk is a pure function of the plan's
// rows (planWalk, walkMove), and validation refuses a checkpoint that
// cannot run before the step that walks back to it. No concurrency test
// here: none of these functions holds state (rollback_db_test.go races a
// real rollback).

// walkRows is chain(n) with step IDs, every step kept with an action, the
// checkpoints set and step fail regressed, reverting to its checkpoint.
func walkRows(n, fail int, checkpoints ...int) Plan {
	p := chain(n, 101)
	for i := range p.Steps {
		s := &p.Steps[i]
		s.ID, s.Checkpoint = int64(i+1), slices.Contains(checkpoints, s.Ordinal)
		s.Runtime = StepRuntime{State: StateKept, Verdict: OutcomeImproved,
			ActionLogID: ptr(int64(100 + i + 1))}
		if s.Ordinal == fail {
			s.OnRegressed = ActionRevertToCheckpoint
			s.Runtime.State, s.Runtime.Verdict = StateReverted, OutcomeRegressed
		}
		if s.Ordinal > fail {
			s.Runtime = StepRuntime{State: StatePending}
		}
	}
	return p
}

func keptOrdinals(w walk) []int {
	out := make([]int, 0, len(w.kept))
	for _, k := range w.kept {
		out = append(out, k.Ordinal)
	}
	return out
}

// Happy path: 5 steps, checkpoint on 2, step 4 regressed and reverted: the
// walk reverts 3 next, then ends at the checkpoint with 1 and 2 kept.
func TestPlanWalk_RevertsBackToTheCheckpoint(t *testing.T) {
	p := walkRows(5, 4, 2)
	w := planWalk(p, p.Steps[3])
	if w.checkpoint != 2 || w.next == nil || w.next.Ordinal != 3 || w.stopped != "" {
		t.Fatalf("walk %+v, want checkpoint 2 and step 3 next", w)
	}
	p.Steps[2].Runtime.State = StateReverted
	w = planWalk(p, p.Steps[3])
	if w.next != nil || w.stopped != "" || !slices.Equal(w.reverted, []int{4, 3}) ||
		len(w.kept) != 0 {
		t.Fatalf("walk %+v, want ended with 4 and 3 reverted, nothing kept in the walk", w)
	}
	m, ok := walkMove(p)
	if !ok || m.kind != moveHalt || m.step.Ordinal != 4 ||
		!strings.Contains(m.reason, "checkpoint step 2") {
		t.Fatalf("walkMove %+v, want a halt naming checkpoint step 2", m)
	}
}

// Dependency rule (the entry's case): 3 depends on 1 and is kept; 2
// regresses and walks back past 1, which a kept step outside the walk
// depends on: 1 is not reverted and the halt says why.
func TestPlanWalk_KeptDependantOutsideTheWalkStopsIt(t *testing.T) {
	p := walkRows(3, 2)
	p.Edges = []Edge{{From: 1, To: 3}}
	p.Steps[2].Runtime = StepRuntime{State: StateKept, Verdict: OutcomeImproved,
		ActionLogID: ptr(int64(103))}
	w := planWalk(p, p.Steps[1])
	if w.next != nil || !strings.Contains(w.stopped, "step 1") ||
		!slices.Equal(keptOrdinals(w), []int{1}) ||
		!strings.Contains(w.kept[0].Why, "kept step 3 depends on it") {
		t.Fatalf("walk %+v, want stopped at 1 because kept step 3 depends on it", w)
	}
	m, ok := walkMove(p)
	if !ok || m.kind != moveHalt || !strings.Contains(m.reason, "kept step 3 depends on it") {
		t.Fatalf("walkMove %+v, want a halt listing step 1 and why", m)
	}
}

// Nil/empty: no checkpoint: the plan's start is the implicit checkpoint,
// so every kept step before the failed one is in the walk.
func TestPlanWalk_NoCheckpointWalksToThePlansStart(t *testing.T) {
	p := walkRows(3, 3)
	w := planWalk(p, p.Steps[2])
	if w.checkpoint != 0 || w.next == nil || w.next.Ordinal != 2 {
		t.Fatalf("walk %+v, want the plan's start and step 2 next", w)
	}
	p.Steps[1].Runtime.State = StateReverted
	if w = planWalk(p, p.Steps[2]); w.next == nil || w.next.Ordinal != 1 {
		t.Fatalf("walk %+v, want step 1 next", w)
	}
	if _, ok := walkMove(Plan{}); ok {
		t.Fatal("an empty plan has no walk")
	}
}

// Boundary: the failed step is the first step (nothing before it), and the
// checkpoint is the step just before the failed one (nothing between).
func TestPlanWalk_Boundaries(t *testing.T) {
	first := walkRows(3, 1)
	if w := planWalk(first, first.Steps[0]); w.next != nil || w.stopped != "" ||
		!slices.Equal(w.reverted, []int{1}) {
		t.Fatalf("first step failed: %+v, want only step 1 reverted", w)
	}
	adjacent := walkRows(4, 3, 2)
	if w := planWalk(adjacent, adjacent.Steps[2]); w.next != nil || w.checkpoint != 2 ||
		!slices.Equal(w.reverted, []int{3}) {
		t.Fatalf("checkpoint just before: %+v, want only step 3 reverted", w)
	}
	// The failed step's own checkpoint flag is not its checkpoint.
	own := walkRows(3, 3, 3)
	if w := planWalk(own, own.Steps[2]); w.checkpoint != 0 || w.next == nil ||
		w.next.Ordinal != 2 {
		t.Fatalf("own checkpoint flag: %+v, want the plan's start", w)
	}
}

// No rollback needed: an ANALYZE step in the walk stays kept with
// no_rollback_needed and the walk goes past it; a forward_fix_only step
// stops it; an unknown action type stops it (fail closed).
func TestPlanWalk_RollbackClasses(t *testing.T) {
	p := walkRows(3, 3)
	p.Steps[1].ActionType, p.Steps[1].ProposedSQL = "analyze_table", "ANALYZE public.orders"
	p.Steps[1].RollbackSQL = ""
	p.Edges = []Edge{{From: 1, To: 2}, {From: 1, To: 3}}
	w := planWalk(p, p.Steps[2])
	if w.next == nil || w.next.Ordinal != 1 || !slices.Equal(keptOrdinals(w), []int{2}) ||
		w.kept[0].Why != ReasonNoRollbackNeeded {
		t.Fatalf("analyze in the walk: %+v, want 2 kept no_rollback_needed and 1 next", w)
	}
	p.Edges = []Edge{{From: 1, To: 2}, {From: 2, To: 3}}
	if w = planWalk(p, p.Steps[2]); w.next != nil || !strings.Contains(w.stopped, "step 2") ||
		!strings.Contains(w.kept[0].Why, "reverted step 3 depends on it") {
		t.Fatalf("analyze a reverted step depends on: %+v, want the walk stopped at 2", w)
	}
	p.Edges = nil
	p.Steps[1].ActionType = "alter_table"
	p.Steps[1].ProposedSQL = "ALTER TABLE public.orders ADD CONSTRAINT c CHECK (id > 0) NOT VALID"
	if w = planWalk(p, p.Steps[2]); w.next != nil ||
		!strings.Contains(w.stopped, "forward_fix_only") {
		t.Fatalf("forward_fix_only in the walk: %+v, want the walk stopped", w)
	}
	p.Steps[1].ActionType, p.Steps[1].ProposedSQL = "mystery", "SELECT 1"
	if w = planWalk(p, p.Steps[2]); w.next != nil || w.stopped == "" {
		t.Fatalf("unknown action type in the walk: %+v, want the walk stopped", w)
	}
}

// Invalid input: an operator step and a kept action without an action log
// row stop the walk; a step that never ran (pending) is not in it.
func TestPlanWalk_StepsItCannotRevert(t *testing.T) {
	p := walkRows(3, 3)
	p.Steps[1] = Step{ID: 2, Ordinal: 2, Kind: KindOperator,
		Runtime: StepRuntime{State: StateKept}}
	if w := planWalk(p, p.Steps[2]); w.next != nil || !strings.Contains(w.stopped, "operator") {
		t.Fatalf("operator step: %+v, want the walk stopped", w)
	}
	p = walkRows(3, 3)
	p.Steps[1].Runtime.ActionLogID = nil
	if w := planWalk(p, p.Steps[2]); w.next != nil || w.stopped == "" {
		t.Fatalf("kept step without an action: %+v, want the walk stopped", w)
	}
	p = walkRows(3, 3)
	p.Steps[1].Runtime = StepRuntime{State: StatePending}
	if w := planWalk(p, p.Steps[2]); w.next == nil || w.next.Ordinal != 1 {
		t.Fatalf("pending step in the walk: %+v, want it passed over and 1 next", w)
	}
}

// State transitions: no walk while a step is in flight, failed or halted;
// a step that expired waiting on a cap and reverts to its checkpoint
// starts one; revert_step never walks.
func TestWalkMove_WhenAWalkRuns(t *testing.T) {
	p := walkRows(3, 3)
	if m, ok := walkMove(p); !ok || m.kind != moveWalkRevert || m.step.Ordinal != 2 {
		t.Fatalf("walkMove %+v, want revert step 2", m)
	}
	for _, st := range []StepState{StateFailed, StateHalted} {
		q := walkRows(3, 3)
		q.Steps[1].Runtime.State = st
		if m, ok := walkMove(q); ok {
			t.Fatalf("a %s step: walkMove %+v, want none (the plan halts)", st, m)
		}
	}
	q := walkRows(3, 3)
	q.Steps[2].OnRegressed = ActionRevertStep
	if m, ok := walkMove(q); ok {
		t.Fatalf("revert_step: walkMove %+v, want no walk", m)
	}
	q = walkRows(3, 3)
	q.Steps[2].OnExpired = ActionRevertToCheckpoint
	q.Steps[2].Runtime = StepRuntime{State: StateSkipped, Verdict: OutcomeExpired}
	if m, ok := walkMove(q); !ok || m.kind != moveWalkRevert || m.step.Ordinal != 2 {
		t.Fatalf("expired waiting: walkMove %+v, want revert step 2", m)
	}
	q.Steps[1].Runtime.State = StateReverting
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	if m := decide(q, now, "a"); m.kind != moveRevert || m.step.Ordinal != 2 {
		t.Fatalf("a reverting step: decide %+v, want its revert resumed", m)
	}
}

// reverseTopo is the reverse of the run order: the lowest runnable ordinal
// first, as the engine starts them.
func TestReverseTopo(t *testing.T) {
	p := walkRows(3, 3)
	p.Edges = []Edge{{From: 1, To: 3}, {From: 3, To: 2}}
	if got := reverseTopo(p); !slices.Equal(got, []int{2, 3, 1}) {
		t.Fatalf("reverseTopo = %v, want [2 3 1]", got)
	}
	p.Edges = nil
	if got := reverseTopo(p); !slices.Equal(got, []int{3, 2, 1}) {
		t.Fatalf("reverseTopo without edges = %v, want [3 2 1]", got)
	}
}

// Invalid input: a step that reverts to its checkpoint where that
// checkpoint must run after it is refused at validation; the same plan
// without the edge, or with revert_step, is clean.
func TestValidate_CheckpointNotBeforeTheStep(t *testing.T) {
	p := threeStepPlan()
	p.Steps[2].Checkpoint = false
	p.Steps[0].Checkpoint = true
	p.Steps[1].OnRegressed = ActionRevertToCheckpoint
	p.Edges = []Edge{{From: 2, To: 1}, {From: 1, To: 3}}
	ps, err := Validate(context.Background(), p, newFakeResolver())
	if err != nil || !hasCode(ps, CodeCheckpointNotBefore, 2) {
		t.Fatalf("problems %v (err %v), want %s@2", codes(ps), err, CodeCheckpointNotBefore)
	}
	p.Edges = []Edge{{From: 1, To: 3}, {From: 2, To: 3}}
	if ps, err = Validate(context.Background(), p, newFakeResolver()); err != nil ||
		hasCode(ps, CodeCheckpointNotBefore, 2) {
		t.Fatalf("checkpoint runs first: problems %v (err %v), want none", codes(ps), err)
	}
	p.Edges = []Edge{{From: 2, To: 1}, {From: 1, To: 3}}
	p.Steps[1].OnRegressed = ActionRevertStep
	if ps, err = Validate(context.Background(), p, newFakeResolver()); err != nil ||
		hasCode(ps, CodeCheckpointNotBefore, 2) {
		t.Fatalf("revert_step: problems %v (err %v), want none", codes(ps), err)
	}
}

// State transitions: the walk moves a kept step to reverting, and a revert
// ends reverted or failed; nothing else leaves kept, and a reverted step
// never moves again.
func TestLegalTransitions_Revert(t *testing.T) {
	for _, c := range []struct {
		from, to StepState
		ok       bool
	}{
		{StateKept, StateReverting, true}, {StateReverting, StateReverted, true},
		{StateReverting, StateFailed, true}, {StateKept, StateReverted, false},
		{StateKept, StateFailed, false}, {StateReverted, StateReverting, false},
		{StateReverting, StateKept, false},
	} {
		if got := legal(c.from, c.to); got != c.ok {
			t.Errorf("legal(%s, %s) = %v, want %v", c.from, c.to, got, c.ok)
		}
	}
}
