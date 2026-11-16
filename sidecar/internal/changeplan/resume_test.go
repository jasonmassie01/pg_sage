package changeplan

import (
	"strings"
	"testing"
	"time"
)

// plan-02e: the resume table decides what a step left in flight by a
// restart or a leader change does next, from what may have happened.
// No concurrent access test here: resumeMove and claimHeldByOther are pure
// functions of their arguments (resume_db_test.go races two engines).

// resumeCase is one row of the resume table.
type resumeCase struct {
	name  string
	state StepState
	log   *int64
	ev    resumeEvidence
	want  resumeKind
	why   string // a part of the reason an unknown decision gives
}

func checkResumeCases(t *testing.T, cases []resumeCase) {
	t.Helper()
	for _, c := range cases {
		s := Step{Ordinal: 2, Runtime: StepRuntime{State: c.state, ActionLogID: c.log}}
		d := resumeMove(s, c.ev)
		if d.kind != c.want {
			t.Errorf("%s: kind %d (%s), want %d", c.name, d.kind, d.reason, c.want)
			continue
		}
		if c.want == resumeUnknown && (d.reason == "" || !strings.Contains(d.reason, c.why)) {
			t.Errorf("%s: reason %q, want it to mention %q", c.name, d.reason, c.why)
		}
	}
}

func TestResumeMove_StartingAndQueued(t *testing.T) {
	checkResumeCases(t, []resumeCase{
		{"starting, no queue item", StateStarting, nil, resumeEvidence{}, resumeRetryStart, ""},
		{"starting, its pending item", StateStarting, nil,
			resumeEvidence{queueID: 4, queueStatus: "pending", queueItems: 1},
			resumeAdoptQueue, ""},
		{"starting, an approved item", StateStarting, nil,
			resumeEvidence{queueID: 4, queueStatus: "approved", queueItems: 1},
			resumeUnknown, "queue item"},
		{"starting, two items", StateStarting, nil,
			resumeEvidence{queueID: 4, queueStatus: "pending", queueItems: 2},
			resumeUnknown, "queue item"},
		{"queued, pending item, no run", StateQueued, nil,
			resumeEvidence{queueID: 4, queueStatus: "pending", queueItems: 1, floorKnown: true},
			resumeRunQueued, ""},
		{"queued, approved item, no run", StateQueued, nil,
			resumeEvidence{queueID: 4, queueStatus: "approved", queueItems: 1, floorKnown: true},
			resumeRunQueued, ""},
		{"queued, a run in the ledger", StateQueued, nil,
			resumeEvidence{queueID: 4, queueStatus: "approved", queueItems: 1, floorKnown: true,
				ledgerID: 9, ledgerRows: 1, ledgerOutcome: "success"},
			resumeUnknown, "ledger"},
		{"queued, no ledger mark", StateQueued, nil,
			resumeEvidence{queueID: 4, queueStatus: "approved", queueItems: 1},
			resumeUnknown, "ledger mark"},
		{"queued, item executed", StateQueued, nil,
			resumeEvidence{queueID: 4, queueStatus: "executed", queueItems: 1, floorKnown: true},
			resumeUnknown, "executed"},
		{"queued, no item", StateQueued, nil, resumeEvidence{floorKnown: true},
			resumeUnknown, "queue item"},
	})
}

func TestResumeMove_RunningExecutedRevertingAndResting(t *testing.T) {
	logID := int64(9)
	at := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	checkResumeCases(t, []resumeCase{
		{"running, its run", StateRunning, nil,
			resumeEvidence{queueID: 4, queueStatus: "approved", queueItems: 1, floorKnown: true,
				ledgerID: 9, ledgerRows: 1, ledgerOutcome: "success", ledgerAt: &at},
			resumeToJudging, ""},
		{"running, its run rolled back", StateRunning, nil,
			resumeEvidence{queueID: 4, floorKnown: true, ledgerID: 9, ledgerRows: 1,
				ledgerOutcome: "rolled_back"},
			resumeToJudging, ""},
		{"running, its run failed", StateRunning, nil,
			resumeEvidence{queueID: 4, floorKnown: true, ledgerID: 9, ledgerRows: 1,
				ledgerOutcome: "failed"},
			resumeFail, ""},
		{"running, no run", StateRunning, nil,
			resumeEvidence{queueID: 4, floorKnown: true}, resumeUnknown, "may or may not"},
		{"running, two runs", StateRunning, nil,
			resumeEvidence{queueID: 4, floorKnown: true, ledgerID: 9, ledgerRows: 2},
			resumeUnknown, "2"},
		{"running, no ledger mark", StateRunning, nil,
			resumeEvidence{queueID: 4, ledgerID: 9, ledgerRows: 1}, resumeUnknown,
			"ledger mark"},
		{"executed, its action", StateExecuted, &logID, resumeEvidence{}, resumeToJudging, ""},
		{"executed, no action", StateExecuted, nil, resumeEvidence{}, resumeUnknown,
			"no action"},
		{"reverting", StateReverting, &logID, resumeEvidence{}, resumeRevert, ""},
		{"judging", StateJudging, &logID, resumeEvidence{}, resumeNone, ""},
		{"pending", StatePending, nil, resumeEvidence{}, resumeNone, ""},
		{"kept", StateKept, &logID, resumeEvidence{}, resumeNone, ""},
		{"awaiting the operator", StateAwaitingOperator, nil, resumeEvidence{}, resumeNone, ""},
		{"awaiting its approval", StateAwaitingApproval, nil, resumeEvidence{}, resumeNone, ""},
	})
}

// Invalid input: a state the resume table does not know (a newer
// version's, read after a downgrade) is never guessed at.
func TestResumeMove_UnknownStateHaltsInterruptedUnknown(t *testing.T) {
	for _, st := range []StepState{"", "launching", "RUNNING", "running "} {
		d := resumeMove(Step{Runtime: StepRuntime{State: st}}, resumeEvidence{floorKnown: true,
			ledgerID: 9, ledgerRows: 1})
		if d.kind != resumeUnknown || !strings.Contains(d.reason, "resume table") {
			t.Errorf("state %q: %+v, want unknown naming the resume table", st, d)
		}
	}
	if knownState("launching") || !knownState(StateRunning) || !knownState(StateAwaitingApproval) {
		t.Fatal("knownState does not match the step vocabulary")
	}
}

// Boundary: another engine's claim one second before it expires holds;
// one second after (and exactly at) it, it does not. A claim of this
// engine's own, or none, never holds it off.
func TestClaimHeldByOther_Boundary(t *testing.T) {
	until := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	s := Step{Runtime: StepRuntime{State: StateRunning, ClaimedBy: "old-leader",
		ClaimedUntil: &until}}
	for _, c := range []struct {
		now   time.Time
		owner string
		want  bool
	}{
		{until.Add(-time.Second), "new-leader", true},
		{until, "new-leader", false},
		{until.Add(time.Second), "new-leader", false},
		{until.Add(-time.Second), "old-leader", false},
	} {
		if got := claimHeldByOther(s, c.now, c.owner); got != c.want {
			t.Errorf("at %s as %s: %v, want %v", c.now.Sub(until), c.owner, got, c.want)
		}
	}
	for _, rt := range []StepRuntime{{State: StateRunning},
		{State: StateRunning, ClaimedBy: "old-leader"},
		{State: StateRunning, ClaimedUntil: &until}} {
		if claimHeldByOther(Step{Runtime: rt}, until.Add(-time.Hour), "new-leader") {
			t.Errorf("%+v: held, want free (no live claim)", rt)
		}
	}
}

// State transitions: resume halts a step it cannot decide from where it
// was left; a judged or ended step is never halted by it.
func TestLegalTransitions_ResumeHalts(t *testing.T) {
	for _, from := range []StepState{StateStarting, StateQueued, StateRunning, StateExecuted,
		StateJudging} {
		if !legal(from, StateHalted) {
			t.Errorf("legal(%s, halted) = false, want true", from)
		}
	}
	for _, from := range []StepState{StatePending, StateKept, StateReverted, StateSkipped,
		StateFailed, StateHalted, StateAwaitingApproval} {
		if legal(from, StateHalted) {
			t.Errorf("legal(%s, halted) = true, want false", from)
		}
	}
}

func TestResumedEngineNeedsResumeOnlyAsLeader(t *testing.T) {
	e := &Engine{needResume: true}
	if !e.needsResume() {
		t.Fatal("a new engine must resume before its first tick")
	}
	e.setNeedResume(false)
	if e.needsResume() {
		t.Fatal("a settled resume must not run again")
	}
	e.setNeedResume(true)
	if !e.needsResume() {
		t.Fatal("losing leadership must make the next leading tick resume")
	}
}
