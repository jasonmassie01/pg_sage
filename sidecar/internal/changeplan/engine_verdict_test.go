package changeplan

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// plan-02a: the verdict wait, the outcome actions and the scheduling
// rules across plans.

// Verdict mapping: each stored verifier outcome maps to the plan's
// vocabulary and its default action; an unknown string halts.
func TestEngine_VerdictMapping(t *testing.T) {
	f := newEngineFixture(t, "engine_verdicts")
	cases := []struct {
		verdict   string
		want      Outcome
		state     StepState
		status    Status
		rollbacks int
	}{
		{"improved", OutcomeImproved, StateKept, StatusCompleted, 0},
		{"neutral", OutcomeNoChange, StateKept, StatusCompleted, 0},
		{"regressed", OutcomeRegressed, StateReverted, StatusHalted, 1},
		{"insufficient_evidence", OutcomeInconclusive, StateHalted, StatusHalted, 0},
		{"unverifiable", OutcomeUnverifiable, StateHalted, StatusHalted, 0},
		{"exploded", OutcomeInconclusive, StateHalted, StatusHalted, 0},
	}
	for i, c := range cases {
		target := int64(101 + i%2)
		p := f.approved(chain(1, target))
		before := f.exec.rollbackCount()
		got := f.runUntil(p.ID, c.verdict, 12)
		s := got.Steps[0].Runtime
		if s.Verdict != c.want || s.State != c.state || got.Status != c.status ||
			f.exec.rollbackCount()-before != c.rollbacks {
			t.Fatalf("%s: plan %s step %s/%s rollbacks %d, want %s %s/%s %d", c.verdict,
				got.Status, s.State, s.Verdict, f.exec.rollbackCount()-before, c.status,
				c.state, c.want, c.rollbacks)
		}
		if c.status == StatusHalted && countKind(f.events(p.ID), EventHalted) != 1 {
			t.Fatalf("%s: want exactly one halted event", c.verdict)
		}
	}
}

// Boundary: a verdict decided one second before the deadline is used; the
// deadline passing one second before the verdict gives expired, and the
// step's on_expired runs.
func TestEngine_DeadlineBoundary(t *testing.T) {
	f := newEngineFixture(t, "engine_deadline")
	run := func(onExpired OutcomeAction, verdictAfter time.Duration) Step {
		in := chain(1, 101)
		in.Steps[0].OnExpired = onExpired
		p := f.approved(in)
		f.tick() // executed at the fixture's clock
		executed := *f.get(p.ID).Steps[0].Runtime.ExecutedAt
		f.tick() // judging
		deadline := executed.Add(60 * time.Minute)
		at := deadline.Add(verdictAfter)
		f.verdicts.set(*f.get(p.ID).Steps[0].Runtime.ActionLogID, "improved", at,
			"monitoring")
		f.clock.Add(deadline.Add(5 * time.Second).Sub(f.clock.Now()))
		f.tick()
		f.tick()
		return f.get(p.ID).Steps[0]
	}
	if s := run(ActionKeep, -time.Second); s.Runtime.Verdict != OutcomeImproved ||
		s.Runtime.State != StateKept {
		t.Fatalf("verdict 1s before the deadline: %+v, want improved and kept", s.Runtime)
	}
	if s := run(ActionKeep, time.Second); s.Runtime.Verdict != OutcomeExpired ||
		s.Runtime.State != StateKept {
		t.Fatalf("deadline 1s before the verdict, on_expired keep: %+v, want expired kept",
			s.Runtime)
	}
	if s := run(ActionHaltAsk, time.Second); s.Runtime.Verdict != OutcomeExpired ||
		s.Runtime.State != StateHalted {
		t.Fatalf("deadline passed, on_expired halt_ask: %+v, want expired and halted",
			s.Runtime)
	}
}

// Verifier revert: a step the verifier reverted (no gain) is reverted with
// reverted_by_verifier, never rolled back again, and its dependant skipped.
func TestEngine_VerifierRevertSkipsTheDependant(t *testing.T) {
	f := newEngineFixture(t, "engine_verifier_revert")
	p := f.approved(chain(2, 101))
	f.tick()
	f.tick()
	f.judgeAll(p.ID, "neutral", "rolled_back")
	for range 6 {
		f.tick()
	}
	got := f.get(p.ID)
	s1, s2 := got.Steps[0].Runtime, got.Steps[1].Runtime
	if s1.State != StateReverted || s1.StateReason != "reverted_by_verifier" ||
		s1.Verdict != OutcomeNoChange || f.exec.rollbackCount() != 0 {
		t.Fatalf("step 1 %+v, rollbacks %d: want reverted_by_verifier, never rolled back",
			s1, f.exec.rollbackCount())
	}
	if s2.State != StateSkipped || s2.StateReason != "predecessor_reverted" ||
		f.manual.callCount() != 1 {
		t.Fatalf("step 2 %+v, runs %d: want skipped predecessor_reverted", s2,
			f.manual.callCount())
	}
	if got.Status != StatusCompleted {
		t.Fatalf("plan %s, want completed (no regression)", got.Status)
	}
}

// A failed rollback of a regressed step fails the step and halts the plan.
func TestEngine_FailedRevertHalts(t *testing.T) {
	f := newEngineFixture(t, "engine_revert_fail")
	f.exec.rollbackErr = errBoom
	p := f.approved(chain(2, 101))
	got := f.runUntil(p.ID, "regressed", 10)
	s1 := got.Steps[0].Runtime
	if got.Status != StatusHalted || s1.State != StateFailed ||
		!strings.Contains(s1.StateReason, errBoom.Error()) ||
		got.Steps[1].Runtime.State != StatePending {
		t.Fatalf("plan %s step 1 %+v: want halted with the rollback error", got.Status, s1)
	}
}

// Interference: two plans whose steps share a target queryid never have
// steps in flight at once (1,000 ticks of a fake clock, checked every
// tick); verdicts arrive at random.
func TestEngine_SharedQueryIDNeverInFlightTwice(t *testing.T) {
	f := newEngineFixture(t, "engine_interference")
	a := f.approved(chain(3, 101))
	b := f.approved(chain(3, 101))
	rng := rand.New(rand.NewSource(7))
	for tick := range 1000 {
		inFlight := 0
		for _, id := range []int64{a.ID, b.ID} {
			for _, s := range f.get(id).Steps {
				if inFlightStates[s.Runtime.State] {
					inFlight++
				}
			}
			if rng.Intn(3) == 0 {
				f.judgeAll(id, "improved", "monitoring")
			}
		}
		if inFlight > 1 {
			t.Fatalf("tick %d: %d steps on queryid 101 in flight", tick, inFlight)
		}
		f.tick()
		f.clock.Add(time.Minute)
	}
	if ga, gb := f.get(a.ID), f.get(b.ID); ga.Status != StatusCompleted ||
		gb.Status != StatusCompleted {
		t.Fatalf("after 1,000 ticks plans %s and %s, want both completed", ga.Status,
			gb.Status)
	}
}

// A setting step waits until no plan step and no executor change is in
// flight, then runs alone.
func TestEngine_SettingStepRunsAlone(t *testing.T) {
	f := newEngineFixture(t, "engine_setting")
	idx := f.approved(chain(1, 101))
	f.tick() // the index step runs
	set := threeStepPlan()
	setting := settingStep(1)
	// A queue item is approvable only for an open finding (the executor's
	// manual path runs findings), so the setting step cites one too.
	setting.FindingID = ptr(int64(13))
	set.Steps, set.Edges = []Step{setting}, nil
	sp := f.approved(set)
	for range 4 {
		f.tick()
	}
	if s := f.get(sp.ID).Steps[0].Runtime; s.State != StatePending {
		t.Fatalf("setting step %s while an index step is in flight, want pending", s.State)
	}
	f.judgeAll(idx.ID, "improved", "monitoring")
	f.waits.anyBusy = true // an executor change elsewhere
	for range 4 {
		f.tick()
	}
	if s := f.get(sp.ID).Steps[0].Runtime; s.State != StatePending {
		t.Fatalf("setting step %s while the executor is busy, want pending", s.State)
	}
	f.waits.anyBusy = false
	f.tick()
	if s := f.get(sp.ID).Steps[0].Runtime; s.State != StateExecuted {
		t.Fatalf("setting step %s once nothing is in flight, want executed", s.State)
	}
	other := f.approved(chain(1, 102))
	for range 3 {
		f.tick()
	}
	if s := f.get(other.ID).Steps[0].Runtime; s.State != StatePending {
		t.Fatalf("an index step started (%s) while a setting step is in flight", s.State)
	}
}

// A step whose objects have a change in flight (the executor's one change
// per object) waits for it.
func TestEngine_BusyObjectsWait(t *testing.T) {
	f := newEngineFixture(t, "engine_busy")
	p := f.approved(chain(1, 101))
	f.waits.pending[p.Steps[0].ProposedSQL] = true
	for range 3 {
		f.tick()
	}
	if s := f.get(p.ID).Steps[0].Runtime; s.State != StatePending {
		t.Fatalf("step %s with its table in flight, want pending", s.State)
	}
	f.waits.pending = map[string]bool{}
	f.tick()
	if s := f.get(p.ID).Steps[0].Runtime; s.State != StateExecuted {
		t.Fatalf("step %s once its table is free, want executed", s.State)
	}
	f.waits.err = fmt.Errorf("catalog read failed")
	q := f.approved(chain(1, 102))
	f.tick()
	if s := f.get(q.ID).Steps[0].Runtime; s.State != StatePending {
		t.Fatalf("step %s when the in-flight lookup fails, want pending (fail closed)",
			s.State)
	}
}
