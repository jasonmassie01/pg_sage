package changeplan

import (
	"strings"
	"testing"
	"time"
)

// plan-02b: approve-all never covers a step that is never automatic. The
// engine stops before it with awaiting_approval, names why, lists it for a
// person and sends one notification; nothing runs it.

// restartPlan is an index step then a restart-bound setting step.
func restartPlan() Plan {
	p := chain(1, 101)
	s := settingStep(2)
	s.Setting = "shared_buffers"
	s.ProposedSQL = "ALTER SYSTEM SET shared_buffers = '256MB'"
	s.RollbackSQL = "ALTER SYSTEM RESET shared_buffers"
	// It cites a finding: a finding-less step cannot be queued (plan-02a).
	s.EvidenceIDs, s.FindingID = nil, ptr(int64(13))
	p.Steps = append(p.Steps, s)
	p.Edges = []Edge{{From: 1, To: 2}}
	return p
}

// CHECK-14: a plan containing a restart-bound setting under approve-all
// stops before that step with awaiting_approval.
func TestAwaitingApproval_RestartBoundSettingStopsThePlan(t *testing.T) {
	f := defaultCaps(t, "await_restart")
	p := f.approved(restartPlan())
	got := f.runUntil(p.ID, "improved", 10)
	for range 5 {
		f.tick()
		f.clock.Add(time.Minute)
	}
	got = f.get(p.ID)
	s1, s2 := got.Steps[0].Runtime, got.Steps[1].Runtime
	if s1.State != StateKept || s2.State != StateAwaitingApproval ||
		!strings.HasPrefix(s2.StateReason, ReasonRestartBound+":") ||
		!strings.Contains(s2.StateReason, "shared_buffers") {
		t.Fatalf("steps %+v / %+v, want step 1 kept and step 2 awaiting_approval "+
			"(restart_bound_setting)", s1, s2)
	}
	if got.Status != StatusRunning || f.manual.callCount() != 1 || len(f.queueItems(p.ID)) != 1 {
		t.Fatalf("plan %s, %d runs, %d queue items: want running with only step 1 run",
			got.Status, f.manual.callCount(), len(f.queueItems(p.ID)))
	}
	assertAskedOnce(t, f, p.ID)
}

// assertAskedOnce: step 2 of plan id is in the inbox once, one notification
// asks for its approval and one event records the move.
func assertAskedOnce(t *testing.T, f *engineFixture, id int64) {
	t.Helper()
	inbox, err := f.store.ListNeedingPerson(f.ctx)
	if err != nil {
		t.Fatalf("ListNeedingPerson: %v", err)
	}
	found := 0
	for _, n := range inbox {
		if n.PlanID == id && n.StepOrdinal == 2 && n.StepState == StateAwaitingApproval &&
			strings.Contains(n.Reason, "shared_buffers") {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("inbox %+v, want step 2 awaiting approval once", inbox)
	}
	var asks int
	for _, evt := range f.notes.sent() {
		if strings.Contains(evt.Subject, "approval") && strings.Contains(evt.Body, "step 2") {
			asks++
		}
	}
	if asks != 1 {
		t.Fatalf("notifications %+v, want one asking to approve step 2", f.notes.sent())
	}
	var moves int
	for _, e := range f.events(id) {
		if e.Detail["to"] == string(StateAwaitingApproval) {
			moves++
		}
	}
	if moves != 1 {
		t.Fatalf("%d awaiting_approval transition events, want 1", moves)
	}
}

// A never-automatic first step stops the plan before anything runs, even
// with every cap and window clear.
func TestAwaitingApproval_FirstStepNeverRuns(t *testing.T) {
	f := defaultCaps(t, "await_first")
	in := restartPlan()
	in.Steps = in.Steps[1:]
	in.Steps[0].Ordinal = 1
	in.Edges = nil
	p := f.approved(in)
	for range 4 {
		f.tick()
	}
	s := f.get(p.ID).Steps[0].Runtime
	if s.State != StateAwaitingApproval || f.manual.callCount() != 0 ||
		len(f.queueItems(p.ID)) != 0 {
		t.Fatalf("step %+v, runs %d: want awaiting_approval with nothing queued", s,
			f.manual.callCount())
	}
}

// State transitions: awaiting_approval is reached only from pending; the
// engine never moves a step out of it (a per-step approval, plan-03a,
// does), and kept to awaiting_approval is illegal.
func TestAwaitingApproval_Transitions(t *testing.T) {
	if !legal(StatePending, StateAwaitingApproval) {
		t.Fatal("pending -> awaiting_approval is not legal")
	}
	for _, from := range []StepState{StateKept, StateRunning, StateJudging, StateQueued} {
		if legal(from, StateAwaitingApproval) {
			t.Errorf("%s -> awaiting_approval is legal", from)
		}
	}
	for _, to := range []StepState{StateStarting, StateRunning, StateKept} {
		if legal(StateAwaitingApproval, to) {
			t.Errorf("awaiting_approval -> %s is legal before plan-03a", to)
		}
	}
	p := chain(2, 101)
	p.Steps[0].Runtime.State = StateAwaitingApproval
	if m := decide(p, time.Now(), "engine-a"); m.kind != moveWait {
		t.Fatalf("decide with a step awaiting approval = %+v, want wait", m)
	}
}
