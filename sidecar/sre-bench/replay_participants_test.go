package srebench

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// rca-07b: the replay report scores participants_named per deadlock case.

// No concurrent access tests: scoring is a pure function of the cases.

const scoredDetail = "Process 1 waits for ShareLock on transaction 9; blocked by process 2.\n" +
	"Process 2 waits for ShareLock on transaction 8; blocked by process 1.\n" +
	"Process 1: UPDATE t SET v = 1\nProcess 2: UPDATE u SET v = 2"

func deadlockCase(id string, blockedBy int) replay.Case {
	return replay.Case{ID: id, Family: "lock_blocking", Class: replay.ClassPositive,
		DeadlockLog: &replay.DeadlockLog{Detail: scoredDetail,
			Participants: []replay.ExpectedParticipant{
				{PID: 1, WaitMode: "ShareLock", BlockedBy: blockedBy},
				{PID: 2, WaitMode: "ShareLock", BlockedBy: 1}}}}
}

// Happy path and boundary: a case the parser names scores 1, one whose
// expected blocker is wrong scores 0, and a case without a deadlock record
// is not scored; JSON and Markdown both carry the scores.
func TestReplayReport_ParticipantsNamed(t *testing.T) {
	cases := []replay.Case{deadlockCase("dl-ok", 2),
		{ID: "no-log", Family: "lock_blocking", Class: replay.ClassPositive},
		deadlockCase("dl-wrong", 3)}
	rep := BuildReplayReport(nil, cases, ReplayMeta{Arms: []string{ArmCausalGraph}})
	want := []ParticipantsScore{{Case: "dl-ok", Named: 1}, {Case: "dl-wrong", Named: 0}}
	if len(rep.ParticipantsNamed) != 2 || rep.ParticipantsNamed[0] != want[0] ||
		rep.ParticipantsNamed[1] != want[1] {
		t.Fatalf("participants_named = %+v, want %+v", rep.ParticipantsNamed, want)
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw),
		`"participants_named":[{"case":"dl-ok","participants_named":1}`) {
		t.Errorf("report JSON lacks the scores: %s", raw)
	}
	md := rep.Markdown()
	for _, line := range []string{"Deadlock participants named", "| dl-ok | 1 |",
		"| dl-wrong | 0 |"} {
		if !strings.Contains(md, line) {
			t.Errorf("markdown lacks %q:\n%s", line, md)
		}
	}
	if strings.Contains(md, "no-log") {
		t.Errorf("markdown scores a case without a deadlock record:\n%s", md)
	}
}

// Nil/empty: no deadlock case gives no section and no JSON key.
func TestReplayReport_NoDeadlockCaseNoScores(t *testing.T) {
	rep := BuildReplayReport(nil, nil, ReplayMeta{})
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ParticipantsNamed != nil || strings.Contains(string(raw), "participants_named") ||
		strings.Contains(rep.Markdown(), "Deadlock participants named") {
		t.Errorf("scores without a deadlock case: %+v", rep.ParticipantsNamed)
	}
}

// CHECK-23 through the bench report: the shipped corpus scores
// lock-deadlock-cycle participants_named = 1.
func TestReplayReport_CorpusScoresDeadlockCycle(t *testing.T) {
	cases, err := replay.Corpus()
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}
	rep := BuildReplayReport(nil, cases, ReplayMeta{Arms: []string{ArmCausalGraph}})
	for _, s := range rep.ParticipantsNamed {
		if s.Case == "lock-deadlock-cycle" {
			if s.Named != 1 {
				t.Errorf("lock-deadlock-cycle participants_named = %d, want 1", s.Named)
			}
			return
		}
	}
	t.Fatalf("lock-deadlock-cycle not scored: %+v", rep.ParticipantsNamed)
}
