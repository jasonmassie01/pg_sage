package srebench

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/sre"
)

// benches-06: CI's release bench loads the expected-scenario manifest
// (SAGE_BENCH_EXPECTED) and fails when a scenario the manifest expects
// on the server's PG major is skipped, or when skips cost a gated arm a
// gate it would have evaluated. Without the variable a run keeps today's
// behaviour: skips are reported, not failed.

const shippedManifest = "testdata/ci-expected.json"

func manifestScenarios() []Scenario {
	return append(positives(sre.TriggerPlan, 2), Scenario{ID: "plan-decoy",
		Family: sre.TriggerPlan, Class: ClassDecoy})
}

func TestParseExpectedReadsOneMajor(t *testing.T) {
	raw := `{"majors": {"17": {"expected": ["plan_regression-pos-1", "plan-decoy"],
		"allowed_skips": {"plan_regression-pos-2": "needs an archive fixture"}}}}`
	m, err := ParseExpected([]byte(raw), manifestScenarios())
	if err != nil {
		t.Fatal(err)
	}
	want, err := m.For(17)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 2 || !want["plan_regression-pos-1"] || !want["plan-decoy"] ||
		want["plan_regression-pos-2"] {
		t.Fatalf("expected on 17 = %v", want)
	}
	if _, err := m.For(16); err == nil || !strings.Contains(err.Error(), "16") {
		t.Fatalf("a major the manifest lacks: err = %v, want one naming 16", err)
	}
}

func TestParseExpectedRefusesAnEmptyManifest(t *testing.T) {
	for name, raw := range map[string]string{"empty file": "", "whitespace": " \n",
		"empty object": "{}", "no majors": `{"majors": {}}`,
		"empty major": `{"majors": {"17": {"expected": []}}}`} {
		_, err := ParseExpected([]byte(raw), manifestScenarios())
		if err == nil || !strings.Contains(err.Error(), "manifest lists no scenarios") {
			t.Errorf("%s: err = %v, want \"manifest lists no scenarios\"", name, err)
		}
	}
}

func TestParseExpectedRefusesInvalidManifests(t *testing.T) {
	cases := map[string]struct{ raw, want string }{
		"unknown expected id": {`{"majors": {"17": {"expected": ["plan_regression-pos-1",
			"plan_regression-pos-2", "plan-decoy", "no-such-scenario"]}}}`,
			`unknown scenario "no-such-scenario"`},
		"unknown allowed skip": {`{"majors": {"17": {"expected": ["plan_regression-pos-1",
			"plan_regression-pos-2", "plan-decoy"], "allowed_skips": {"ghost": "why"}}}}`,
			`unknown scenario "ghost"`},
		"scenario left out": {`{"majors": {"17": {"expected": ["plan_regression-pos-1",
			"plan-decoy"]}}}`, `"plan_regression-pos-2" is neither expected nor`},
		"listed twice": {`{"majors": {"17": {"expected": ["plan_regression-pos-1",
			"plan_regression-pos-2", "plan-decoy"],
			"allowed_skips": {"plan-decoy": "why"}}}}`, `"plan-decoy" is listed twice`},
		"duplicate expected": {`{"majors": {"17": {"expected": ["plan_regression-pos-1",
			"plan_regression-pos-1", "plan_regression-pos-2", "plan-decoy"]}}}`,
			`"plan_regression-pos-1" is listed twice`},
		"skip without reason": {`{"majors": {"17": {"expected": ["plan_regression-pos-1",
			"plan_regression-pos-2"], "allowed_skips": {"plan-decoy": " "}}}}`,
			`"plan-decoy" has no reason`},
		"bad major": {`{"majors": {"seventeen": {"expected": ["plan-decoy"]}}}`,
			`major "seventeen"`},
		"unknown field": {`{"majors": {"17": {"expect": ["plan-decoy"]}}}`,
			`unknown field`},
		"not json": {`majors: 17`, `parse`},
	}
	for name, c := range cases {
		_, err := ParseExpected([]byte(c.raw), manifestScenarios())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, c.want)
		}
	}
}

func TestLoadExpectedNamesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	if _, err := LoadExpected(path, Scenarios()); err == nil ||
		!strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want one naming %s", err, path)
	}
}

func TestShippedManifestCoversEveryScenarioOnEveryMajor(t *testing.T) {
	m, err := LoadExpected(shippedManifest, Scenarios())
	if err != nil {
		t.Fatal(err)
	}
	for major := 14; major <= 18; major++ {
		want, err := m.For(major)
		if err != nil {
			t.Fatalf("PG%d: %v", major, err)
		}
		// CI's image has no archive fixture (archive_mode is off), so the
		// archiver program is the one allowed skip; everything else counts.
		if want["wal-archiver-failure"] {
			t.Errorf("PG%d expects wal-archiver-failure, which CI cannot run", major)
		}
		if len(want) != len(Scenarios())-1 {
			t.Errorf("PG%d expects %d of %d scenarios", major, len(want), len(Scenarios()))
		}
	}
}

func TestExpectedSkipsNamesTheScenarioAndItsReason(t *testing.T) {
	ss := positives(sre.TriggerReplicationLag, 2)
	arms := []string{ArmCausalGraph, ArmLLM}
	results := runAll(ss, arms, 1, 3,
		map[string]string{ss[1].ID: "wal_level is replica, not logical"})
	got := ExpectedSkips(map[string]bool{ss[0].ID: true, ss[1].ID: true}, results)
	if len(got) != 1 {
		t.Fatalf("violations = %v, want one line for the skipped scenario", got)
	}
	if !strings.Contains(got[0], ss[1].ID) ||
		!strings.Contains(got[0], "wal_level is replica, not logical") ||
		!strings.Contains(got[0], "6 run(s)") {
		t.Fatalf("violation = %q, want the id, the Unsupported reason and 6 runs", got[0])
	}
	// A skip the manifest allows (not expected) is not a violation.
	if got := ExpectedSkips(map[string]bool{ss[0].ID: true}, results); len(got) != 0 {
		t.Fatalf("an allowed skip failed the check: %v", got)
	}
	if got := ExpectedSkips(nil, results); len(got) != 0 {
		t.Fatalf("no manifest (local run) failed the check: %v", got)
	}
}

func TestLostGatesFlagsGatesSkipsLeftWithoutData(t *testing.T) {
	pos := positives(sre.TriggerReplicationLag, 1)
	decoy := Scenario{ID: "repl-decoy", Family: sre.TriggerReplicationLag,
		Class: ClassDecoy}
	arms := []string{ArmCausalGraph}
	results := runAll(append(pos, decoy), arms, 1, 1,
		map[string]string{decoy.ID: "wal_level is replica, not logical"})
	s := Summarize(results, arms)
	gates := EvaluateGates(s, ArmCausalGraph)
	lost := LostGates(gates, results, arms)
	byID := map[string]GateResult{}
	for _, g := range lost {
		byID[g.ID] = g
	}
	if _, ok := byID[GateAbstention]; !ok {
		t.Fatalf("lost = %v, want R1-ABSTAIN (its only decoy was skipped)", lost)
	}
	if _, ok := byID[GateDecoy]; !ok {
		t.Fatalf("lost = %v, want CHECK-42-DECOY", lost)
	}
	if _, ok := byID[GateNoise]; ok {
		t.Fatalf("CHECK-42-NOISE flagged: the family has no noise scenario at all")
	}
	if len(lost) != 2 {
		t.Fatalf("lost = %v, want exactly the two decoy-fed gates", lost)
	}
}

func TestLostGatesIgnoresStructuralGates(t *testing.T) {
	pos := positives(sre.TriggerPlan, 2)
	arms := []string{ArmCausalGraph}
	results := runAll(pos, arms, 1, 1, nil)
	s := Summarize(results, arms)
	// No skips: the not-evaluated gates (no decoys, the pooled human-review
	// gates) are structural and stay logged, not failed.
	if lost := LostGates(EvaluateGates(s, ArmCausalGraph), results, arms); len(lost) != 0 {
		t.Fatalf("lost = %v, want none without skips", lost)
	}
	// The fake model's §12 gates are never evaluated, skips or not.
	skipped := runAll(pos, []string{ArmLLM}, 1, 1, map[string]string{pos[0].ID: "x"})
	fake := ModelArmGates(Summarize(skipped, []string{ArmLLM}), skipped, ArmLLM, LLMFake)
	if lost := LostGates(fake, skipped, []string{ArmLLM}); len(lost) != 0 {
		t.Fatalf("fake-model gates flagged: %v", lost)
	}
	// An ungated arm's gates never fail the bench.
	if lost := LostGates(EvaluateGates(s, ArmCausalGraph), results, nil); len(lost) != 0 {
		t.Fatalf("ungated arm flagged: %v", lost)
	}
}

// A family whose only sufficient-evidence scenario was skipped loses its
// top-1 gate even though a decoy still scored: the decoy feeds other gates,
// not R1-TOP1.
func TestLostGatesFlagsTop1WhenEveryPositiveWasSkipped(t *testing.T) {
	pos := positives(sre.TriggerPlan, 1)
	decoy := Scenario{ID: "plan-decoy", Family: sre.TriggerPlan, Class: ClassDecoy}
	arms := []string{ArmCausalGraph}
	results := runAll(append(pos, decoy), arms, 1, 2,
		map[string]string{pos[0].ID: "pg_stat_statements not loaded"})
	lost := LostGates(EvaluateGates(Summarize(results, arms), ArmCausalGraph), results, arms)
	got := map[string]bool{}
	for _, g := range lost {
		if g.Family != string(sre.TriggerPlan) || g.Arm != ArmCausalGraph {
			t.Fatalf("lost gate %+v, want plan_regression on %s", g, ArmCausalGraph)
		}
		got[g.ID] = true
	}
	if !got[GateTop1] || !got[GateDecoy] || len(got) != 2 {
		t.Fatalf("lost = %v, want exactly R1-TOP1 and CHECK-42-DECOY", lost)
	}
}

// The fake model's §12 gates are not evaluated by design, so they are not
// lost even when every run of the family was skipped.
func TestLostGatesIgnoresFakeModelGatesWhenEveryRunSkipped(t *testing.T) {
	pos := positives(sre.TriggerPlan, 2)
	arms := []string{ArmLLM}
	skipped := runAll(pos, arms, 1, 1, map[string]string{pos[0].ID: "x", pos[1].ID: "y"})
	fake := ModelArmGates(Summarize(skipped, arms), skipped, ArmLLM, LLMFake)
	if lost := LostGates(fake, skipped, arms); len(lost) != 0 {
		t.Fatalf("fake-model gates flagged: %v", lost)
	}
	live := EvaluateGates(Summarize(skipped, arms), ArmLLM)
	if lost := LostGates(live, skipped, arms); len(lost) == 0 {
		t.Fatal("a live-model arm whose every run was skipped lost no gate")
	}
}

// CI's release bar equals the earned-autonomy promotion bar: the shipped
// report must carry n >= MinTop1N per family.
func TestCIReleaseMinRunsMatchesThePromotionBar(t *testing.T) {
	bar := earned.DefaultThresholds().MinTop1N
	wf := readCIWorkflow(t)
	tagValue := regexp.MustCompile(`startsWith\(github\.ref, 'refs/tags/v'\) && '([^']+)'`)
	// The signed release report is PG17's (ci-03b: the bench matrix legs).
	steps := benchLegs(wf)[benchJob+" pg17"]
	if len(steps) == 0 {
		t.Fatal("no PG17 legs in the bench matrix")
	}
	for _, st := range steps {
		if v, ok := st.Env[EnvRepeats]; ok {
			t.Errorf("%s still sets %s=%q, which conflicts with %s", st.Name, EnvRepeats, v,
				EnvMinRunsPerFamily)
		}
		m := tagValue.FindStringSubmatch(st.Env[EnvMinRunsPerFamily])
		if m == nil {
			t.Errorf("%s: %s=%q, want a value on tags", st.Name, EnvMinRunsPerFamily,
				st.Env[EnvMinRunsPerFamily])
			continue
		}
		if n, err := strconv.Atoi(m[1]); err != nil || n != bar {
			t.Errorf("%s: release min runs %q, want the promotion bar %d", st.Name, m[1], bar)
		}
		e := tagValue.FindStringSubmatch(st.Env[EnvExpected])
		if e == nil || e[1] != shippedManifest {
			t.Errorf("%s: %s=%q, want %s on tags", st.Name, EnvExpected,
				st.Env[EnvExpected], shippedManifest)
		}
	}
	if _, err := os.Stat(shippedManifest); err != nil {
		t.Fatalf("CI names %s: %v", shippedManifest, err)
	}
}

func TestReportCarriesTheSampleSizeAgainstTheBar(t *testing.T) {
	plan, lock := positives(sre.TriggerPlan, 3), positives(sre.TriggerLock, 5)
	arms := []string{ArmCausalGraph}
	results := append(runAll(plan, arms, 1, 4, map[string]string{plan[0].ID: "x"}),
		runAll(lock, arms, 1, 2, nil)...)
	r := BuildReport(results, ReportMeta{Arms: arms, Gated: arms, Repeats: 4,
		MinRunsPerFamily: 10})
	got := map[string]FamilyRunRecord{}
	for _, f := range r.FamilyRuns {
		got[f.Family] = f
	}
	if p := got[string(sre.TriggerPlan)]; p.N != 8 || p.Bar != 10 {
		t.Fatalf("plan = %+v, want n 8 against bar 10", p)
	}
	if l := got[string(sre.TriggerLock)]; l.N != 10 || l.Bar != 10 {
		t.Fatalf("lock = %+v, want n 10 against bar 10", l)
	}
	md := r.Markdown()
	if !strings.Contains(md, "- plan_regression: n=8 against bar 10") ||
		!strings.Contains(md, "- lock_blocking: n=10 against bar 10") {
		t.Fatalf("markdown lacks the per-family n lines:\n%s", md)
	}
	if strings.Contains(md, "Top-up passes") {
		t.Fatalf("a run without top-up passes reports some:\n%s", md)
	}
	topped := BuildReport(append(results, runAll(plan[1:], arms, 5, 5, nil)...),
		ReportMeta{Arms: arms, Gated: arms, Repeats: 4, MinRunsPerFamily: 10})
	md = topped.Markdown()
	if !strings.Contains(md, "Top-up passes 5 to 5 reran") ||
		!strings.Contains(md, "- plan_regression: n=10 against bar 10") {
		t.Fatalf("topped-up markdown:\n%s", md)
	}
	plain := BuildReport(results, ReportMeta{Arms: arms, Gated: arms, Repeats: 4})
	if len(plain.FamilyRuns) != 0 || strings.Contains(plain.Markdown(), "against bar") {
		t.Fatal("a run without a bar reports a sample size against one")
	}
}
