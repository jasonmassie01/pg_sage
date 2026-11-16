package changeplan

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// plan-02c: the engine reverts along the plan's graph. A regressed step
// whose action is revert_to_checkpoint is reverted, then every kept step
// back to the latest checkpoint before it, newest first; a step a kept
// step outside the walk depends on is not reverted, and the plan halts
// with what was kept and why. These tests use the fake executor and
// verifier over a real database (engine_fake_test.go); the reverts' read
// back is the real catalog's.

// walkChain is chain(n, 101) with the checkpoints set and step fail
// reverting to its checkpoint on a regression.
func walkChain(n, fail int, checkpoints ...int) Plan {
	p := chain(n, 101)
	for i := range p.Steps {
		s := &p.Steps[i]
		s.Checkpoint = slices.Contains(checkpoints, s.Ordinal)
		if s.Ordinal == fail {
			s.OnRegressed = ActionRevertToCheckpoint
		}
	}
	return p
}

// runVerdicts ticks plan id a minute apart until it is terminal or halted,
// giving each judging step the verdict its ordinal names (improved when
// none is named).
func (f *engineFixture) runVerdicts(id int64, verdicts map[int]string, maxTicks int) Plan {
	f.t.Helper()
	for range maxTicks {
		for _, s := range f.get(id).Steps {
			if s.Runtime.State != StateJudging || s.Runtime.ActionLogID == nil {
				continue
			}
			v := verdicts[s.Ordinal]
			if v == "" {
				v = "improved"
			}
			f.verdicts.set(*s.Runtime.ActionLogID, v, f.clock.Now(), "monitoring")
		}
		f.tick()
		f.clock.Add(time.Minute)
		if p := f.get(id); p.Status.Terminal() || p.Status == StatusHalted {
			return p
		}
	}
	return f.get(id)
}

// rolledBack is the ordinals of p whose actions the fake executor was
// asked to roll back, in call order.
func (f *engineFixture) rolledBack(p Plan) []int {
	f.t.Helper()
	byAction := map[int64]int{}
	for _, s := range p.Steps {
		if s.Runtime.ActionLogID != nil {
			byAction[*s.Runtime.ActionLogID] = s.Ordinal
		}
	}
	f.exec.mu.Lock()
	defer f.exec.mu.Unlock()
	var out []int
	for _, id := range f.exec.rollbacks {
		if o, ok := byAction[id]; ok {
			out = append(out, o)
		}
	}
	return out
}

func states(p Plan) []StepState {
	out := make([]StepState, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, s.Runtime.State)
	}
	return out
}

// haltReason is the reason of plan id's one halted event.
func (f *engineFixture) haltedReason(id int64) string {
	f.t.Helper()
	ev := f.events(id)
	if n := countKind(ev, EventHalted); n != 1 {
		f.t.Fatalf("plan %d has %d halted events, want 1: %v", id, n, eventKinds(ev))
	}
	for _, e := range ev {
		if e.Kind == EventHalted {
			return fmt.Sprint(e.Detail["reason"])
		}
	}
	return ""
}

// Happy path: 5 steps, checkpoint after 2, step 4 regresses with
// revert_to_checkpoint: 4 and 3 are reverted in that order, 1 and 2 kept,
// 5 never starts; every revert is an event with its action_log id.
func TestRevertTo_HappyPathWalksBackToTheCheckpoint(t *testing.T) {
	f := newEngineFixture(t, "rollback_happy")
	p := f.approved(walkChain(5, 4, 2))
	got := f.runVerdicts(p.ID, map[int]string{4: "regressed"}, 60)
	want := []StepState{StateKept, StateKept, StateReverted, StateReverted, StatePending}
	if got.Status != StatusHalted || !slices.Equal(states(got), want) {
		t.Fatalf("plan %s steps %v, want halted with %v", got.Status, states(got), want)
	}
	if rb := f.rolledBack(got); !slices.Equal(rb, []int{4, 3}) {
		t.Fatalf("rolled back %v, want [4 3]", rb)
	}
	if f.manual.callCount() != 4 || got.Steps[3].Runtime.Verdict != OutcomeRegressed {
		t.Fatalf("%d runs, step 4 verdict %s: want 4 runs and regressed",
			f.manual.callCount(), got.Steps[3].Runtime.Verdict)
	}
	if why := f.haltedReason(p.ID); !strings.Contains(why, "checkpoint step 2") {
		t.Fatalf("halt reason %q, want it to name checkpoint step 2", why)
	}
	if n := len(f.notes.sent()); n != 1 {
		t.Fatalf("%d notifications, want exactly 1 for the halt", n)
	}
	f.checkRevertEvents(got, 3, StateKept)
	f.checkRevertEvents(got, 4, StateJudging)
}

// checkRevertEvents: step ordinal's revert wrote from -> reverting and
// reverting -> reverted events, the last carrying its action_log id
// (state transitions: kept to reverting to reverted, with their events).
func (f *engineFixture) checkRevertEvents(p Plan, ordinal int, from StepState) {
	f.t.Helper()
	s := p.Steps[ordinal-1]
	var started, done bool
	for _, e := range f.events(p.ID) {
		if e.StepID == nil || *e.StepID != s.ID {
			continue
		}
		switch {
		case e.Detail["from"] == string(from) && e.Detail["to"] == string(StateReverting):
			started = true
		case e.Detail["from"] == string(StateReverting) &&
			e.Detail["to"] == string(StateReverted):
			done = e.Kind == EventStepReverted &&
				fmt.Sprint(e.Detail["action_log_id"]) == fmt.Sprint(*s.Runtime.ActionLogID)
		}
	}
	if !started || !done {
		f.t.Fatalf("step %d events: %s->reverting %v, reverting->reverted with action %d %v",
			ordinal, from, started, *s.Runtime.ActionLogID, done)
	}
}

// Dependency rule: 3 depends on 1 and runs before 2 (2 depends on 3); 2
// regresses with revert_to_checkpoint back past 1: 1 is not reverted
// because kept 3 depends on it, and the halt lists it with why.
func TestRevertTo_KeptDependantKeepsItsPredecessor(t *testing.T) {
	f := newEngineFixture(t, "rollback_dependency")
	in := walkChain(3, 2)
	in.Edges = []Edge{{From: 1, To: 3}, {From: 3, To: 2}}
	p := f.approved(in)
	got := f.runVerdicts(p.ID, map[int]string{2: "regressed"}, 40)
	want := []StepState{StateKept, StateReverted, StateKept}
	if got.Status != StatusHalted || !slices.Equal(states(got), want) {
		t.Fatalf("plan %s steps %v, want halted with %v", got.Status, states(got), want)
	}
	if rb := f.rolledBack(got); !slices.Equal(rb, []int{2}) {
		t.Fatalf("rolled back %v, want only [2]", rb)
	}
	why := f.haltedReason(p.ID)
	if !strings.Contains(why, "step 1") || !strings.Contains(why, "kept step 3 depends on it") {
		t.Fatalf("halt reason %q, want step 1 kept because kept step 3 depends on it", why)
	}
	if r := got.Steps[0].Runtime.StateReason; !strings.Contains(r, "kept step 3 depends on it") {
		t.Fatalf("step 1 state reason %q, want why it was kept", r)
	}
}

// Nil/empty: no checkpoint in the plan: every kept step before the failed
// one is reverted, newest first (the plan's start is the checkpoint).
func TestRevertTo_NoCheckpointRevertsEveryKeptStep(t *testing.T) {
	f := newEngineFixture(t, "rollback_no_checkpoint")
	p := f.approved(walkChain(3, 3))
	got := f.runVerdicts(p.ID, map[int]string{3: "regressed"}, 40)
	want := []StepState{StateReverted, StateReverted, StateReverted}
	if got.Status != StatusHalted || !slices.Equal(states(got), want) {
		t.Fatalf("plan %s steps %v, want halted with %v", got.Status, states(got), want)
	}
	if rb := f.rolledBack(got); !slices.Equal(rb, []int{3, 2, 1}) {
		t.Fatalf("rolled back %v, want [3 2 1]", rb)
	}
	if why := f.haltedReason(p.ID); !strings.Contains(why, "the plan's start") {
		t.Fatalf("halt reason %q, want the plan's start named as the checkpoint", why)
	}
}

// Error propagation: RollbackAction failing on step 3 stops the walk at 3,
// marks it failed with the error, halts, and leaves 1 and 2 as they were.
func TestRevertTo_RollbackFailureStopsTheWalk(t *testing.T) {
	f := newEngineFixture(t, "rollback_failure")
	p := f.approved(walkChain(4, 4))
	f.exec.rollbackFailSQL = map[string]error{p.Steps[2].ProposedSQL: errBoom}
	got := f.runVerdicts(p.ID, map[int]string{4: "regressed"}, 40)
	want := []StepState{StateKept, StateKept, StateFailed, StateReverted}
	if got.Status != StatusHalted || !slices.Equal(states(got), want) {
		t.Fatalf("plan %s steps %v, want halted with %v", got.Status, states(got), want)
	}
	if r := got.Steps[2].Runtime.StateReason; !strings.Contains(r, errBoom.Error()) {
		t.Fatalf("step 3 reason %q, want the rollback's error", r)
	}
	if rb := f.rolledBack(got); !slices.Equal(rb, []int{4, 3}) {
		t.Fatalf("rolled back %v, want [4 3] and nothing after the failure", rb)
	}
	failed := false
	for _, e := range f.events(p.ID) {
		failed = failed || (e.StepID != nil && *e.StepID == got.Steps[2].ID &&
			e.Detail["from"] == string(StateReverting) && e.Detail["to"] == string(StateFailed))
	}
	if !failed {
		t.Fatal("want a reverting -> failed event for step 3")
	}
}

// Boundary: the failed step is the first step (only it is reverted); the
// checkpoint is the step just before the failed one (only it is reverted).
func TestRevertTo_Boundaries(t *testing.T) {
	f := newEngineFixture(t, "rollback_boundary")
	p := f.approved(walkChain(3, 1))
	got := f.runVerdicts(p.ID, map[int]string{1: "regressed"}, 20)
	if want := []StepState{StateReverted, StatePending, StatePending}; got.Status !=
		StatusHalted || !slices.Equal(states(got), want) {
		t.Fatalf("first step: plan %s steps %v, want halted with %v", got.Status,
			states(got), want)
	}
	q := walkChain(4, 3, 2)
	for i := range q.Steps {
		q.Steps[i].TargetQueryIDs = []int64{102}
	}
	q.Scope.QueryIDs = []int64{102}
	p = f.approved(q)
	got = f.runVerdicts(p.ID, map[int]string{3: "regressed"}, 40)
	if want := []StepState{StateKept, StateKept, StateReverted, StatePending}; got.Status !=
		StatusHalted || !slices.Equal(states(got), want) {
		t.Fatalf("checkpoint just before: plan %s steps %v, want halted with %v",
			got.Status, states(got), want)
	}
	if rb := f.rolledBack(got); !slices.Equal(rb, []int{3}) {
		t.Fatalf("checkpoint just before: rolled back %v, want [3]", rb)
	}
}

// No rollback needed: an ANALYZE step inside the walk is kept with
// no_rollback_needed, not rolled back, and the walk goes on past it.
func TestRevertTo_AnalyzeIsKeptWithNoRollbackNeeded(t *testing.T) {
	f := newEngineFixture(t, "rollback_analyze")
	in := walkChain(3, 3)
	in.Steps[1].ActionType, in.Steps[1].ProposedSQL = "analyze_table", "ANALYZE public.orders"
	in.Steps[1].RollbackSQL = ""
	in.Edges = []Edge{{From: 1, To: 2}, {From: 1, To: 3}}
	p := f.approved(in)
	got := f.runVerdicts(p.ID, map[int]string{3: "regressed"}, 40)
	want := []StepState{StateReverted, StateKept, StateReverted}
	if got.Status != StatusHalted || !slices.Equal(states(got), want) {
		t.Fatalf("plan %s steps %v, want halted with %v", got.Status, states(got), want)
	}
	if rb := f.rolledBack(got); !slices.Equal(rb, []int{3, 1}) {
		t.Fatalf("rolled back %v, want [3 1] (never the ANALYZE)", rb)
	}
	if r := got.Steps[1].Runtime.StateReason; r != ReasonNoRollbackNeeded {
		t.Fatalf("ANALYZE step reason %q, want %s", r, ReasonNoRollbackNeeded)
	}
	if why := f.haltedReason(p.ID); !strings.Contains(why, ReasonNoRollbackNeeded) {
		t.Fatalf("halt reason %q, want the ANALYZE step listed as kept", why)
	}
}

// A forward_fix_only step in the walk (it ran with its own approval,
// plan-03a) stops the walk with the plan halted. Validation refuses such
// SQL in a plan today, so step 2 runs as an ANALYZE and its row is then
// rewritten to the forward-fix action its own approval would have run.
func TestRevertTo_ForwardFixOnlyStepHaltsTheWalk(t *testing.T) {
	f := newEngineFixture(t, "rollback_forward_fix")
	in := walkChain(3, 3)
	in.Steps[1].ActionType, in.Steps[1].ProposedSQL = "analyze_table", "ANALYZE public.orders"
	in.Steps[1].RollbackSQL = ""
	p := f.approved(in)
	mid := f.runVerdicts(p.ID, nil, 8) // steps 1 and 2 kept, 3 judging
	if mid.Steps[2].Runtime.State != StateJudging {
		t.Fatalf("steps %v, want step 3 judging", states(mid))
	}
	mustExecPlan(t, f, fmt.Sprintf(`UPDATE sage.plan_step SET action_type = 'alter_table',
		proposed_sql = %s WHERE id = %d`, quoteSQL("ALTER TABLE public.orders ADD "+
		"CONSTRAINT c CHECK (id > 0) NOT VALID"), mid.Steps[1].ID))
	got := f.runVerdicts(p.ID, map[int]string{3: "regressed"}, 40)
	want := []StepState{StateKept, StateKept, StateReverted}
	if got.Status != StatusHalted || !slices.Equal(states(got), want) {
		t.Fatalf("plan %s steps %v, want halted with %v", got.Status, states(got), want)
	}
	if rb := f.rolledBack(got); !slices.Equal(rb, []int{3}) {
		t.Fatalf("rolled back %v, want only [3]", rb)
	}
	if why := f.haltedReason(p.ID); !strings.Contains(why, "forward_fix_only") {
		t.Fatalf("halt reason %q, want the forward_fix_only step named", why)
	}
}

func quoteSQL(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Concurrency (deterministic half; rollback_db_test.go races the real
// executor): an operator already rolled step 2's action back outside the
// plan: the walk does not roll it back again, and records it reverted.
func TestRevertTo_ActionRolledBackOutsideThePlan(t *testing.T) {
	f := newEngineFixture(t, "rollback_outside")
	p := f.approved(walkChain(3, 3))
	f.runVerdicts(p.ID, nil, 8) // steps 1 and 2 kept, 3 judging
	mid := f.get(p.ID)
	if mid.Steps[1].Runtime.State != StateKept {
		t.Fatalf("steps %v, want step 2 kept", states(mid))
	}
	mustExecPlan(t, f, fmt.Sprintf(`UPDATE sage.action_log SET outcome = 'rolled_back'
		WHERE id = %d`, *mid.Steps[1].Runtime.ActionLogID))
	got := f.runVerdicts(p.ID, map[int]string{3: "regressed"}, 40)
	if want := []StepState{StateReverted, StateReverted, StateReverted}; !slices.Equal(
		states(got), want) || got.Status != StatusHalted {
		t.Fatalf("plan %s steps %v, want halted with %v", got.Status, states(got), want)
	}
	if rb := f.rolledBack(got); !slices.Equal(rb, []int{3, 1}) {
		t.Fatalf("rolled back %v, want [3 1]: step 2 was already rolled back", rb)
	}
	if r := got.Steps[1].Runtime.StateReason; !strings.Contains(r, ReasonRevertedOutside) {
		t.Fatalf("step 2 reason %q, want %s", r, ReasonRevertedOutside)
	}
}

// A revert whose landing does not read back (the index is still there)
// fails the step and halts; it is never recorded reverted.
func TestRevertTo_RevertThatDoesNotLandFails(t *testing.T) {
	f := newEngineFixture(t, "rollback_not_landed")
	mustExecPlan(t, f, `CREATE TABLE public.orders (c1 int, c2 int);
		CREATE INDEX idx_101_1 ON public.orders (c1)`)
	p := f.approved(walkChain(2, 2))
	got := f.runVerdicts(p.ID, map[int]string{2: "regressed"}, 30)
	if want := []StepState{StateFailed, StateReverted}; !slices.Equal(states(got), want) ||
		got.Status != StatusHalted {
		t.Fatalf("plan %s steps %v, want halted with %v", got.Status, states(got), want)
	}
	if r := got.Steps[0].Runtime.StateReason; !strings.Contains(r, "did not land") ||
		!strings.Contains(r, "idx_101_1") {
		t.Fatalf("step 1 reason %q, want the index that is still there", r)
	}
}

// Fail closed: a setting step's revert whose action recorded no prior
// value cannot be read back, so it fails rather than claim reverted.
func TestRevertTo_SettingWithoutAPriorFailsClosed(t *testing.T) {
	f := newEngineFixture(t, "rollback_setting")
	in := chain(1, 101)
	in.Steps = []Step{settingStep(1)}
	in.Steps[0].FindingID = ptr(int64(11)) // a step runs only with an open finding
	in.Steps[0].OnUnverifiable = ActionKeep
	in.Steps[0].OnRegressed = ActionRevertStep
	in.Scope.QueryIDs = nil
	p := f.approved(in)
	got := f.runVerdicts(p.ID, map[int]string{1: "regressed"}, 20)
	if got.Steps[0].Runtime.State != StateFailed || got.Status != StatusHalted ||
		!strings.Contains(got.Steps[0].Runtime.StateReason, "prior") {
		t.Fatalf("plan %s step %+v, want failed on the missing prior and halted",
			got.Status, got.Steps[0].Runtime)
	}
}

// Invalid input: RevertTo refuses a mode that is not a revert.
func TestRevertTo_RefusesOtherModes(t *testing.T) {
	f := newEngineFixture(t, "rollback_mode")
	p := f.approved(chain(1, 101))
	for _, mode := range []OutcomeAction{ActionKeep, ActionHaltAsk, "sideways"} {
		err := f.engine.RevertTo(f.ctx, p, p.Steps[0], mode)
		if !errors.Is(err, ErrRevertMode) || f.exec.rollbackCount() != 0 {
			t.Fatalf("RevertTo(%q) = %v with %d rollbacks, want ErrRevertMode and none",
				mode, err, f.exec.rollbackCount())
		}
	}
}
