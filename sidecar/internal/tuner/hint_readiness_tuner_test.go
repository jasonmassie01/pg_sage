package tuner

import (
	"context"
	"sync"
	"testing"
	"time"
)

// tuning-04: the tuner gates executable hint SQL on application effect,
// names the missing prerequisite on the finding, and re-reads the facts
// each cycle so turning enable_hint_table on needs no restart.

func readinessCandidate() candidate {
	return candidate{QueryID: 4404, Query: "SELECT * FROM t WHERE a = $1",
		MeanExecTime: 150, Calls: 100}
}

const readinessHint = "IndexScan(t t_a_idx)"

func TestBuildFinding_NotReadyNamesThePrerequisite(t *testing.T) {
	cases := []struct {
		name string
		hp   *HintPlanAvailability
		want string
	}{
		{"enable_hint_table off", &HintPlanAvailability{Preload: true, HintTableReady: true,
			Privilege: true, Reasons: []string{reasonHintTableOff}}, reasonHintTableOff},
		{"two prerequisites", &HintPlanAvailability{Preload: true,
			Reasons: []string{reasonHintTableOff, reasonLayout}},
			reasonHintTableOff + "; " + reasonLayout},
		{"never detected", nil, reasonNotDetected},
		{"table ready but no application effect, no reason recorded",
			&HintPlanAvailability{HintTableReady: true}, reasonNotDetected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tu := &Tuner{hintPlan: tc.hp}
			f := tu.buildFinding(context.Background(), readinessCandidate(), nil,
				readinessHint, "title", "rationale")
			if f.RecommendedSQL != "" || f.RollbackSQL != "" {
				t.Fatalf("not-ready finding carries SQL: %q / %q",
					f.RecommendedSQL, f.RollbackSQL)
			}
			if got := f.Detail["hint_readiness"]; got != tc.want {
				t.Fatalf("hint_readiness = %v, want %q", got, tc.want)
			}
			if f.Detail["hint_directive"] != readinessHint || f.ActionRisk != "safe" {
				t.Fatalf("advisory finding lost its hint: %+v", f.Detail)
			}
		})
	}
}

func TestBuildFinding_ReadyCarriesSQLAndNoReadinessNote(t *testing.T) {
	tu := &Tuner{hintPlan: &HintPlanAvailability{ApplicationEffect: true,
		HintTableReady: true}}
	f := tu.buildFinding(context.Background(), readinessCandidate(), nil,
		readinessHint, "title", "rationale")
	if f.RecommendedSQL != BuildInsertSQL(4404, readinessHint) ||
		f.RollbackSQL != BuildDeleteSQL(4404, readinessHint) {
		t.Fatalf("ready finding SQL = %q / %q", f.RecommendedSQL, f.RollbackSQL)
	}
	if _, ok := f.Detail["hint_readiness"]; ok {
		t.Fatalf("ready finding carries hint_readiness: %v", f.Detail["hint_readiness"])
	}
}

// An informational finding (no hint) has nothing to install, so no note.
func TestBuildFinding_EmptyHintHasNoReadinessNote(t *testing.T) {
	tu := &Tuner{hintPlan: &HintPlanAvailability{Reasons: []string{reasonHintTableOff}}}
	f := tu.buildFinding(context.Background(), readinessCandidate(), nil, " ", "t", "r")
	if _, ok := f.Detail["hint_readiness"]; ok || f.RecommendedSQL != "" {
		t.Fatalf("informational finding: %+v sql=%q", f.Detail, f.RecommendedSQL)
	}
}

func probedTuner(db *fakeHintDB, start *HintPlanAvailability) *Tuner {
	tu := New(nil, TunerConfig{}, start, noopLogFn)
	tu.hintSource = db
	return tu
}

// State transitions: off, then on in the next cycle, then a failed read.
func TestRefreshHintPlan_OffThenOnWithoutRestart(t *testing.T) {
	db := &fakeHintDB{}
	ready := tableRow(true, true, true)
	db.set(settingsRow(sharedWithHints, "", "off"), ready)
	start, err := ProbeHintReadiness(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	withSessionLoad(start, true)
	tu := probedTuner(db, start)
	tu.refreshHintPlan(context.Background())
	if tu.HintsAvailable() {
		t.Fatal("hints available with enable_hint_table off")
	}

	db.set(settingsRow(sharedWithHints, "", "off", dbSetting("", true, tableOnConf)), ready)
	tu.refreshHintPlan(context.Background())
	if !tu.HintsAvailable() {
		t.Fatalf("ALTER DATABASE SET on not picked up: %+v", tu.hints())
	}
	if !tu.hints().SessionLoad {
		t.Fatal("refresh dropped the startup SessionLoad fact")
	}
	f := tu.buildFinding(context.Background(), readinessCandidate(), nil,
		readinessHint, "t", "r")
	if f.RecommendedSQL != BuildInsertSQL(4404, readinessHint) {
		t.Fatalf("after turning it on: SQL %q", f.RecommendedSQL)
	}

	db.set(settingsRow(sharedWithHints, "", "on"), fakeHintRow{err: context.DeadlineExceeded})
	tu.refreshHintPlan(context.Background())
	if tu.HintsAvailable() {
		t.Fatal("a failed re-read kept hints available")
	}
	f = tu.buildFinding(context.Background(), readinessCandidate(), nil,
		readinessHint, "t", "r")
	if f.RecommendedSQL != "" || f.Detail["hint_readiness"] != reasonProbeFailed {
		t.Fatalf("after a failed read: sql=%q readiness=%v",
			f.RecommendedSQL, f.Detail["hint_readiness"])
	}
}

// Facts supplied by a caller (not probed) are never re-read.
func TestRefreshHintPlan_SuppliedFactsAreKept(t *testing.T) {
	db := &fakeHintDB{}
	tu := probedTuner(db, &HintPlanAvailability{ApplicationEffect: true,
		HintTableReady: true})
	tu.refreshHintPlan(context.Background())
	if len(db.sqls) != 0 || !tu.HintsAvailable() {
		t.Fatalf("supplied facts re-probed: %d statements, available=%t",
			len(db.sqls), tu.HintsAvailable())
	}
	var nilTuner *Tuner
	if nilTuner.HintsAvailable() {
		t.Fatal("nil tuner reports hints available")
	}
}

// Concurrent access: the tuning agent asks HintsAvailable from its own
// goroutine while the tuner cycle re-reads the facts. (-race is not
// available on this machine; this exercises the lock and checks the final
// state.)
func TestRefreshHintPlan_ConcurrentReaders(t *testing.T) {
	db := &fakeHintDB{}
	ready := tableRow(true, true, true)
	db.set(settingsRow(sharedWithHints, "", "off"), ready)
	start, _ := ProbeHintReadiness(context.Background(), db)
	tu := probedTuner(db, start)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = tu.HintsAvailable()
					if h := tu.hints(); h == nil {
						t.Error("hints() returned nil mid-refresh")
						return
					}
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		conf := tableOffConf
		if i%2 == 1 {
			conf = tableOnConf
		}
		db.set(settingsRow(sharedWithHints, "", "off", dbSetting("", true, conf)), ready)
		tu.refreshHintPlan(context.Background())
	}
	close(stop)
	wg.Wait()
	if !tu.HintsAvailable() {
		t.Fatal("last refresh (on) not visible")
	}
	if d := time.Since(tu.hints().CheckedAt); d < 0 || d > time.Minute {
		t.Fatalf("CheckedAt not refreshed: %v ago", d)
	}
}
