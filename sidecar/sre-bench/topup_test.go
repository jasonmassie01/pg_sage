package srebench

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre"
)

// benches-06: a release run tops up the scored scenarios of every family
// that skips left short of the promotion bar's n, and fails when a family
// still falls short. Skipped and errored runs never count.

// positives is n uniquely named sufficient-evidence scenarios of family.
func positives(family sre.TriggerKind, n int) []Scenario {
	out := make([]Scenario, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, Scenario{ID: fmt.Sprintf("%s-pos-%d", family, i), Family: family,
			Class: ClassPositive, Gold: Gold{Root: "root"}})
	}
	return out
}

// runAll is every scenario run on every arm for repeats first..last; a
// scenario in skip is skipped with its reason.
func runAll(ss []Scenario, arms []string, first, last int,
	skip map[string]string) []Result {
	var out []Result
	for rep := first; rep <= last; rep++ {
		for _, sc := range ss {
			for _, arm := range arms {
				out = append(out, Result{Scenario: sc, Arm: arm, Repeat: rep,
					Skipped: skip[sc.ID]})
			}
		}
	}
	return out
}

func ids(ss []Scenario) []string {
	out := make([]string, 0, len(ss))
	for _, sc := range ss {
		out = append(out, sc.ID)
	}
	return out
}

func TestTopUpReturnsTheScoredScenariosOfAShortFamily(t *testing.T) {
	plan := positives(sre.TriggerPlan, 3)
	arms := []string{ArmCausalGraph, ArmLLM}
	skip := map[string]string{plan[1].ID: "max_prepared_transactions is 0"}
	results := runAll(plan, arms, 1, 4, skip)
	if n := FamilyTop1Runs(results, plan, arms)[string(sre.TriggerPlan)]; n != 8 {
		t.Fatalf("n before the top-up = %d, want 8 (2 scored scenarios x 4)", n)
	}
	rerun, unreachable := topUp(results, plan, arms, 10)
	want := []string{plan[0].ID, plan[2].ID}
	if got := ids(rerun); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rerun = %v, want the 2 scored scenarios %v", got, want)
	}
	if len(unreachable) != 0 {
		t.Fatalf("unreachable = %v, want none", unreachable)
	}
	results = append(results, runAll(rerun, arms, 5, 5, nil)...)
	if n := FamilyTop1Runs(results, plan, arms)[string(sre.TriggerPlan)]; n != 10 {
		t.Fatalf("n after one top-up round = %d, want 10", n)
	}
	if rerun, _ := topUp(results, plan, arms, 10); len(rerun) != 0 {
		t.Fatalf("a family at the bar is topped up again: %v", ids(rerun))
	}
}

func TestTopUpLeavesOutErroredRunsAndOtherFamilies(t *testing.T) {
	plan, lock := positives(sre.TriggerPlan, 3), positives(sre.TriggerLock, 6)
	all := append(append([]Scenario{}, plan...), lock...)
	arms := []string{ArmCausalGraph}
	results := runAll(all, arms, 1, 2, nil)
	results[0].Err = fmt.Errorf("inject: boom")
	counts := FamilyTop1Runs(results, all, arms)
	if counts[string(sre.TriggerPlan)] != 5 || counts[string(sre.TriggerLock)] != 12 {
		t.Fatalf("counts = %v, want plan 5 (one errored run left out) and lock 12", counts)
	}
	rerun, _ := topUp(results, all, arms, 10)
	for _, sc := range rerun {
		if sc.Family != sre.TriggerPlan {
			t.Fatalf("rerun %s of family %s, which is at the bar", sc.ID, sc.Family)
		}
	}
	if len(rerun) != 3 {
		t.Fatalf("rerun = %v, want the 3 plan scenarios", ids(rerun))
	}
}

func TestTopUpCountsTheWeakestGatedArm(t *testing.T) {
	plan := positives(sre.TriggerPlan, 2)
	results := runAll(plan, []string{ArmCausalGraph}, 1, 5, nil)
	results = append(results, runAll(plan, []string{ArmLLM}, 1, 4, nil)...)
	arms := []string{ArmCausalGraph, ArmLLM}
	if n := FamilyTop1Runs(results, plan, arms)[string(sre.TriggerPlan)]; n != 8 {
		t.Fatalf("n = %d, want the LLM arm's 8, not the causal graph's 10", n)
	}
	// Derived and ungated arms do not decide.
	if n := FamilyTop1Runs(results, plan, []string{ArmCausalGraph})[string(
		sre.TriggerPlan)]; n != 10 {
		t.Fatalf("causal graph alone n = %d, want 10", n)
	}
}

func TestTopUpReportsAFamilyWithEveryScenarioSkippedAsUnreachable(t *testing.T) {
	repl := positives(sre.TriggerReplicationLag, 3)
	skip := map[string]string{}
	for _, sc := range repl {
		skip[sc.ID] = "wal_level is replica, not logical"
	}
	arms := []string{ArmCausalGraph}
	results := runAll(repl, arms, 1, 4, skip)
	rerun, unreachable := topUp(results, repl, arms, 10)
	if len(rerun) != 0 {
		t.Fatalf("rerun = %v: a family with nothing scored would loop", ids(rerun))
	}
	if len(unreachable) != 1 || unreachable[0] != string(sre.TriggerReplicationLag) {
		t.Fatalf("unreachable = %v, want [replication_lag]", unreachable)
	}
}

// No concurrent access test: topUp and FamilyTop1Runs are pure functions
// of their arguments; TestTopUpIsSafeForParallelReaders checks that they
// do not mutate the shared results.
func TestTopUpIsSafeForParallelReaders(t *testing.T) {
	plan := positives(sre.TriggerPlan, 3)
	arms := []string{ArmCausalGraph}
	results := runAll(plan, arms, 1, 2, map[string]string{plan[0].ID: "no"})
	var wg sync.WaitGroup
	got := make([]int, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rerun, _ := topUp(results, plan, arms, 10)
			got[i] = len(rerun)
		}(i)
	}
	wg.Wait()
	for i, n := range got {
		if n != 2 {
			t.Fatalf("reader %d: rerun %d scenarios, want 2", i, n)
		}
	}
	if results[0].Skipped != "no" || len(results) != 6 {
		t.Fatalf("topUp mutated its input: %+v", results[0])
	}
}

func TestTopUpEmptyAndZeroInputsAskForNothing(t *testing.T) {
	plan := positives(sre.TriggerPlan, 3)
	arms := []string{ArmCausalGraph}
	cases := map[string]func() ([]Scenario, []string){
		"no results":   func() ([]Scenario, []string) { return topUp(nil, plan, arms, 10) },
		"no scenarios": func() ([]Scenario, []string) { return topUp(nil, nil, arms, 10) },
		"no gated arm": func() ([]Scenario, []string) {
			return topUp(runAll(plan, arms, 1, 1, nil), plan, nil, 10)
		},
		"zero bar": func() ([]Scenario, []string) {
			return topUp(runAll(plan, arms, 1, 1, nil), plan, arms, 0)
		},
		"decoys only": func() ([]Scenario, []string) {
			d := scenariosOf(sre.TriggerWAL, 0, 2)
			return topUp(runAll(d, arms, 1, 1, nil), d, arms, 10)
		},
	}
	for name, call := range cases {
		rerun, unreachable := call()
		if name == "no results" {
			// Nothing ran: every family is unreachable, nothing reruns.
			if len(rerun) != 0 || len(unreachable) != 1 {
				t.Errorf("%s: rerun %v unreachable %v", name, ids(rerun), unreachable)
			}
			continue
		}
		if len(rerun) != 0 || len(unreachable) != 0 {
			t.Errorf("%s: rerun %v unreachable %v, want nothing", name, ids(rerun),
				unreachable)
		}
	}
}

func TestTopUpRunsContinuesRepeatsUntilTheBar(t *testing.T) {
	plan := positives(sre.TriggerPlan, 3)
	arms := []string{ArmCausalGraph}
	skip := map[string]string{plan[1].ID: "skipped"}
	results := runAll(plan, arms, 1, 4, skip)
	var reps []int
	run := func(ss []Scenario, rep int) []Result {
		reps = append(reps, rep)
		return runAll(ss, arms, rep, rep, skip)
	}
	results = topUpRuns(results, plan, arms, 12, 4, run)
	if fmt.Sprint(reps) != "[5 6]" {
		t.Fatalf("top-up repeats = %v, want [5 6] continuing after repeat 4", reps)
	}
	if n := FamilyTop1Runs(results, plan, arms)[string(sre.TriggerPlan)]; n != 12 {
		t.Fatalf("n = %d, want 12", n)
	}
}

func TestTopUpRunsStopsAtMaxRepeats(t *testing.T) {
	plan := positives(sre.TriggerPlan, 1)
	arms := []string{ArmCausalGraph}
	results := runAll(plan, arms, 1, 1, nil)
	var last int
	run := func(ss []Scenario, rep int) []Result {
		last = rep
		return runAll(ss, arms, rep, rep, nil)
	}
	results = topUpRuns(results, plan, arms, 1000, 1, run)
	if last != MaxRepeats {
		t.Fatalf("last top-up repeat = %d, want MaxRepeats %d", last, MaxRepeats)
	}
	short := ShortFamilies(results, plan, arms, 1000)
	if len(short) != 1 || !strings.Contains(short[0], "n=10") ||
		!strings.Contains(short[0], "bar 1000") {
		t.Fatalf("short = %v, want plan_regression at n=10 against bar 1000", short)
	}
}

func TestTopUpRunsNeverLoopsOnAnUnreachableFamily(t *testing.T) {
	plan := positives(sre.TriggerPlan, 2)
	arms := []string{ArmCausalGraph}
	skip := map[string]string{plan[0].ID: "a", plan[1].ID: "b"}
	calls := 0
	run := func(ss []Scenario, rep int) []Result { calls++; return nil }
	results := topUpRuns(runAll(plan, arms, 1, 4, skip), plan, arms, 10, 4, run)
	if calls != 0 {
		t.Fatalf("ran %d top-up rounds for a family with nothing scored", calls)
	}
	short := ShortFamilies(results, plan, arms, 10)
	if len(short) != 1 || !strings.Contains(short[0], "unreachable") {
		t.Fatalf("short = %v, want the family named unreachable", short)
	}
}

func TestShortFamiliesAtTheReleaseBar(t *testing.T) {
	arms := []string{ArmCausalGraph}
	ten := positives(sre.TriggerPlan, 10)
	nine := positives(sre.TriggerLock, 9)
	results := append(runAll(ten, arms, 1, 1, nil), runAll(nine, arms, 1, 1, nil)...)
	all := append(append([]Scenario{}, ten...), nine...)
	short := ShortFamilies(results, all, arms, 10)
	if len(short) != 1 {
		t.Fatalf("short = %v, want only lock_blocking", short)
	}
	if !strings.Contains(short[0], string(sre.TriggerLock)) ||
		!strings.Contains(short[0], "n=9") || !strings.Contains(short[0], "bar 10") {
		t.Fatalf("short = %q, want lock_blocking n=9 against bar 10", short[0])
	}
	if got := ShortFamilies(results, all, arms, 0); len(got) != 0 {
		t.Fatalf("no bar (min runs unset) reported %v", got)
	}
}

func TestParseMinRuns(t *testing.T) {
	for v, want := range map[string]int{"": 0, " ": 0, "10": 10, " 1 ": 1, "250": 250} {
		got, err := ParseMinRuns(v)
		if err != nil || got != want {
			t.Errorf("ParseMinRuns(%q) = %d, %v; want %d", v, got, err, want)
		}
	}
	for _, v := range []string{"0", "-1", "ten", "1.5", "10x"} {
		if _, err := ParseMinRuns(v); err == nil ||
			!strings.Contains(err.Error(), EnvMinRunsPerFamily) {
			t.Errorf("ParseMinRuns(%q) err = %v, want an error naming %s", v, err,
				EnvMinRunsPerFamily)
		}
	}
}

func TestBenchRepeatsFromTheEnvironment(t *testing.T) {
	plan := positives(sre.TriggerPlan, 3)
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	repeats, minRuns, err := BenchRepeats(env(nil), plan)
	if err != nil || repeats != 1 || minRuns != 0 {
		t.Fatalf("unset = %d %d %v, want today's single pass", repeats, minRuns, err)
	}
	repeats, minRuns, err = BenchRepeats(env(map[string]string{EnvRepeats: "3"}), plan)
	if err != nil || repeats != 3 || minRuns != 0 {
		t.Fatalf("repeats=3 gave %d %d %v", repeats, minRuns, err)
	}
	repeats, minRuns, err = BenchRepeats(env(map[string]string{EnvMinRunsPerFamily: "10"}),
		plan)
	if err != nil || repeats != 4 || minRuns != 10 {
		t.Fatalf("min runs 10 over 3 scenarios = %d %d %v, want 4 repeats", repeats,
			minRuns, err)
	}
	_, _, err = BenchRepeats(env(map[string]string{EnvMinRunsPerFamily: "10",
		EnvRepeats: "4"}), plan)
	if err == nil || !strings.Contains(err.Error(), EnvRepeats) ||
		!strings.Contains(err.Error(), EnvMinRunsPerFamily) {
		t.Fatalf("both set: err = %v, want a conflict naming both", err)
	}
	if _, _, err = BenchRepeats(env(map[string]string{EnvMinRunsPerFamily: "x"}),
		plan); err == nil {
		t.Fatal("an invalid min runs was accepted")
	}
}

// A top-up pass continues the repeat numbers of the passes before it, so
// its runs are distinct records in the report.
func TestRunNumbersATopUpPassAfterThePassesBefore(t *testing.T) {
	prog := &fakeProgram{manifestErr: errTest}
	cfg := fakeConfig(2, &fakeArm{name: "fake"})
	cfg.FirstRepeat = 5
	rs := Run(context.Background(), &Env{}, []Scenario{lockScenarioWith(prog)}, cfg)
	if len(rs) != 6 || prog.injects != 2 {
		t.Fatalf("results %d, injects %d, want 2 passes of 3 arms", len(rs), prog.injects)
	}
	for i, r := range rs {
		if want := 5 + i/3; r.Repeat != want {
			t.Fatalf("result %d repeat %d, want %d", i, r.Repeat, want)
		}
	}
	cfg.FirstRepeat = 0
	if rs := Run(context.Background(), &Env{}, []Scenario{lockScenarioWith(prog)},
		cfg); rs[0].Repeat != 1 || rs[len(rs)-1].Repeat != 2 {
		t.Fatalf("unset FirstRepeat numbered %d..%d, want 1..2", rs[0].Repeat,
			rs[len(rs)-1].Repeat)
	}
}
