package changeplan

import (
	"testing"
	"time"
)

// plan-02a: decide, judgeOutcome and the transition table are pure
// functions of their inputs. No concurrency test: none holds state.

func TestJudgeOutcome_Table(t *testing.T) {
	exec := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := Step{DeadlineMinutes: 60, Runtime: StepRuntime{ExecutedAt: &exec}}
	deadline := exec.Add(time.Hour)
	at := func(d time.Duration) *time.Time { v := deadline.Add(d); return &v }
	cases := []struct {
		name    string
		v       ActionVerdict
		now     time.Time
		want    Outcome
		byVer   bool
		decided bool
	}{
		{"pending before the deadline", ActionVerdict{Verdict: "pending"},
			deadline.Add(-time.Minute), "", false, false},
		{"no outcome yet", ActionVerdict{}, deadline, "", false, false},
		{"pending after the deadline", ActionVerdict{Verdict: "pending"},
			deadline.Add(time.Second), OutcomeExpired, false, true},
		{"verdict exactly at the deadline", ActionVerdict{Verdict: "improved",
			DecidedAt: at(0)}, deadline.Add(time.Minute), OutcomeImproved, false, true},
		{"verdict after the deadline", ActionVerdict{Verdict: "improved",
			DecidedAt: at(time.Second)}, deadline.Add(time.Minute), OutcomeExpired, false, true},
		{"verifier rolled back", ActionVerdict{Verdict: "neutral",
			ActionOutcome: "rolled_back", DecidedAt: at(-time.Hour)}, exec,
			OutcomeNoChange, true, true},
		{"rolled back with no verdict", ActionVerdict{ActionOutcome: "rolled_back"}, exec,
			OutcomeInconclusive, true, true},
		{"monitor expired", ActionVerdict{ActionOutcome: "verification_expired"}, exec,
			OutcomeExpired, false, true},
		{"unknown verdict", ActionVerdict{Verdict: "exploded", DecidedAt: at(-time.Hour)},
			exec, OutcomeInconclusive, false, true},
	}
	for _, c := range cases {
		o, byVer, done := judgeOutcome(s, c.v, c.now)
		if o != c.want || byVer != c.byVer || done != c.decided {
			t.Errorf("%s: (%q, %v, %v), want (%q, %v, %v)", c.name, o, byVer, done, c.want,
				c.byVer, c.decided)
		}
	}
}

func TestLegalTransitions(t *testing.T) {
	for _, c := range []struct {
		from, to StepState
		ok       bool
	}{
		{StatePending, StateStarting, true}, {StateStarting, StateQueued, true},
		{StateQueued, StateRunning, true}, {StateRunning, StateExecuted, true},
		{StateExecuted, StateJudging, true}, {StateJudging, StateKept, true},
		{StateJudging, StateReverting, true}, {StateReverting, StateReverted, true},
		{StateKept, StateRunning, false}, {StatePending, StateRunning, false},
		{StateReverted, StatePending, false}, {StateFailed, StateStarting, false},
		{StateExecuted, StateKept, false}, {"", StateStarting, false},
	} {
		if got := legal(c.from, c.to); got != c.ok {
			t.Errorf("legal(%s, %s) = %v, want %v", c.from, c.to, got, c.ok)
		}
	}
}

func TestDecide_OrderAndClaims(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	p := chain(3, 101)
	for i := range p.Steps {
		p.Steps[i].ID = int64(i + 1)
		p.Steps[i].Runtime.State = StatePending
	}
	if m := decide(p, now, "a"); m.kind != moveStart || m.step.Ordinal != 1 {
		t.Fatalf("fresh plan: %+v, want start step 1", m)
	}
	p.Steps[0].Runtime = StepRuntime{State: StateExecuted, ClaimedBy: "b",
		ClaimedUntil: &later}
	if m := decide(p, now, "a"); m.kind != moveWait {
		t.Fatalf("another engine's live claim: %+v, want wait", m)
	}
	if m := decide(p, later, "a"); m.kind != moveJudgeStart {
		t.Fatalf("a lapsed claim: %+v, want judge start", m)
	}
	p.Steps[0].Runtime = StepRuntime{State: StateKept, Verdict: OutcomeRegressed}
	if m := decide(p, now, "a"); m.kind != moveHalt {
		t.Fatalf("a kept regression: %+v, want halt", m)
	}
	p.Steps[0].Runtime = StepRuntime{State: StateSkipped}
	if m := decide(p, now, "a"); m.kind != moveSkip || m.step.Ordinal != 2 ||
		m.reason != ReasonPredecessorSkipped {
		t.Fatalf("a skipped predecessor: %+v, want skip step 2", m)
	}
	for i := range p.Steps {
		p.Steps[i].Runtime = StepRuntime{State: StateKept}
	}
	if m := decide(p, now, "a"); m.kind != moveComplete {
		t.Fatalf("all kept: %+v, want complete", m)
	}
	empty := Plan{}
	if m := decide(empty, now, "a"); m.kind != moveComplete {
		t.Fatalf("no steps: %+v, want complete", m)
	}
}

func TestIsSetting(t *testing.T) {
	for sql, want := range map[string]bool{
		"ALTER SYSTEM SET work_mem = '8MB'":             true,
		"alter system reset work_mem":                   true,
		"ALTER DATABASE app SET random_page_cost = 1.1": true,
		"ALTER ROLE app RESET work_mem":                 true,
		"ALTER DATABASE app RENAME TO app2":             false,
		"CREATE INDEX CONCURRENTLY i ON public.t (a)":   false,
		"ALTER TABLE public.t SET (fillfactor = 90)":    false,
		"  ALTER USER app SET statement_timeout = '5s'": true,
	} {
		if got := isSetting(Step{ProposedSQL: sql}); got != want {
			t.Errorf("isSetting(%q) = %v, want %v", sql, got, want)
		}
	}
}
