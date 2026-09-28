package srebench

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Scoring (AI-SRE-SPEC §12): per family and pooled, precision and recall
// of the supported hypotheses (root plus contributing) against the gold
// mechanisms, top-1 on scenarios with a known cause, and abstention on
// scenarios where the right answer is "inconclusive".

var errTest = errors.New("fault program failed")

func result(family sre.TriggerKind, gold Gold, out Outcome) Result {
	return Result{Scenario: Scenario{ID: "s", Family: family, Gold: gold}, Outcome: out}
}

func TestScore_PerfectRun(t *testing.T) {
	rs := []Result{
		result(sre.TriggerLock, Gold{Root: "a", Contributing: []string{"b"}},
			Outcome{State: sre.StateConcluded, Root: "a", Contributing: []string{"b"}}),
		result(sre.TriggerLock, Gold{}, Outcome{State: sre.StateInconclusive}),
	}
	fams, pooled := ScoreResults(rs)
	lock := fams[string(sre.TriggerLock)]
	if pooled.TP != 2 || pooled.FP != 0 || pooled.FN != 0 || pooled.Precision() != 1 ||
		pooled.Recall() != 1 || pooled.Top1() != 1 || pooled.Abstention() != 1 ||
		lock.N != 2 {
		t.Fatalf("pooled %+v lock %+v", pooled, lock)
	}
}

func TestScore_WrongRootFalsePositiveAndMissedAbstention(t *testing.T) {
	rs := []Result{
		result(sre.TriggerWAL, Gold{Root: "slot"},
			Outcome{State: sre.StateConcluded, Root: "surge", Contributing: []string{"slot"}}),
		result(sre.TriggerWAL, Gold{}, Outcome{State: sre.StateConcluded, Root: "surge"}),
		result(sre.TriggerConnections, Gold{Root: "leak"}, Outcome{State: sre.StateInconclusive}),
	}
	fams, pooled := ScoreResults(rs)
	wal := fams[string(sre.TriggerWAL)]
	if wal.TP != 1 || wal.FP != 2 || wal.FN != 0 || wal.Top1() != 0 || wal.Abstention() != 0 {
		t.Fatalf("wal = %+v", wal)
	}
	conn := fams[string(sre.TriggerConnections)]
	if conn.FN != 1 || conn.Top1() != 0 || !math.IsNaN(conn.Precision()) {
		t.Fatalf("connections = %+v (precision %v)", conn, conn.Precision())
	}
	if pooled.Precision() != 1.0/3 || pooled.Recall() != 0.5 {
		t.Fatalf("pooled precision %v recall %v", pooled.Precision(), pooled.Recall())
	}
}

// Skipped scenarios (fixture unavailable) and harness errors are
// reported, never scored as right or wrong.
func TestScore_SkippedAndErroredAreNotScored(t *testing.T) {
	rs := []Result{
		{Scenario: Scenario{ID: "arch", Family: sre.TriggerWAL, Gold: Gold{Root: "x"}},
			Skipped: "archive_mode is off"},
		{Scenario: Scenario{ID: "bad", Family: sre.TriggerWAL, Gold: Gold{Root: "x"}},
			Err: errTest},
	}
	_, pooled := ScoreResults(rs)
	if pooled.N != 0 || pooled.Top1Total != 0 || !math.IsNaN(pooled.Top1()) {
		t.Fatalf("pooled = %+v", pooled)
	}
	report := Report(rs)
	for _, want := range []string{"arch", "skipped: archive_mode is off", "bad", "error",
		"n/a"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report lacks %q:\n%s", want, report)
		}
	}
}

func TestScenarios_AreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	families := map[sre.TriggerKind]int{}
	for _, sc := range Scenarios() {
		if sc.ID == "" || seen[sc.ID] || sc.Program == nil {
			t.Fatalf("scenario %+v is malformed or duplicated", sc)
		}
		seen[sc.ID] = true
		families[sc.Family]++
		switch sc.Class {
		case ClassPositive, ClassDecoy:
			if sc.Gold.Root == "" {
				t.Errorf("%s has no gold root", sc.ID)
			}
		case ClassBenign:
			if sc.Gold.Root != "" {
				t.Errorf("benign %s has a gold root", sc.ID)
			}
		default:
			t.Errorf("%s has class %q", sc.ID, sc.Class)
		}
	}
	for _, f := range []sre.TriggerKind{sre.TriggerLock, sre.TriggerConnections,
		sre.TriggerWAL, sre.TriggerPlan} {
		if families[f] < 3 {
			t.Errorf("family %s has %d scenarios, want at least 3", f, families[f])
		}
	}
}
