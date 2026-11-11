package srebench

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "sre-bench"))
}

// Bench budgets: one scenario (every live arm) in one pass, so the pass
// budget grows with the scenarios selected (SAGE_BENCH_FAMILIES); a fixed
// allowance for set-up, recovery and the report, which every shard had
// before benches-11 and which a shard of slow families (checkpoint storms
// average over 30 s a scenario) needs to finish its last scenario; the
// replay corpus (only with SAGE_BENCH_REPLAY=1); and the extra time a
// live model may take (up to 2 turns of 50 s per investigation, though
// real replies take seconds).
const (
	scenarioBudget  = 30 * time.Second
	harnessBudget   = 3 * time.Minute
	replayBudget    = 3 * time.Minute
	liveModelBudget = 60 * time.Minute
)

// benchBudget bounds the whole bench run; replay adds the corpus replay.
func benchBudget(repeats, scenarios int, llm LLMConfig, replay bool) time.Duration {
	d := time.Duration(repeats*scenarios)*scenarioBudget + harnessBudget
	if replay {
		d += replayBudget
	}
	if llm.Mode == LLMLive {
		d += liveModelBudget
	}
	return d
}

// TestPGIncidentBench runs every scenario's fault program on real
// PostgreSQL through each ready live arm (the causal graph with the LLM
// off; the LLM-on arm with the fake adversarial model or a live one),
// derives the always-escalate and rules-only baselines from the same
// evidence, and writes the JSON and Markdown report to
// SAGE_BENCH_REPORT_DIR (default: the test's temp dir). It fails when a
// fault program breaks or a live arm misses a pre-registered gate it is
// evaluated on; gates without the data to evaluate them are reported as
// not evaluated. With SAGE_BENCH_MIN_RUNS_PER_FAMILY it tops up the
// families skips left short and fails on one still short; with
// SAGE_BENCH_EXPECTED (CI on tags) it fails when an expected scenario is
// skipped or skips cost a gated arm a gate (benches-06).
func TestPGIncidentBench(t *testing.T) {
	if !BenchRunRequested(os.Getenv) {
		t.Skipf("set %s=1 to run the full bench (CI runs it in its own step)", EnvRun)
	}
	dsn := testdb.SkipUnlessLive(t)
	b := planBench(t)
	ctx, cancel := context.WithTimeout(context.Background(),
		benchBudget(b.budgetRepeats(), len(b.selected), b.llm, b.replay))
	t.Cleanup(cancel)
	env := NewEnv(ctx, t, dsn)
	var version string
	var versionNum int
	if err := env.Pool.QueryRow(ctx, "SELECT version(), "+
		"current_setting('server_version_num')::int").Scan(&version, &versionNum); err != nil {
		t.Fatalf("server version: %v", err)
	}
	expected := benchExpected(t, versionNum/10000)
	results := b.run(ctx, env)
	sageVersion, sageCommit := BuildFromEnv(os.Getenv)
	report := BuildReport(results, ReportMeta{Arms: b.cfg.ArmNames(), Gated: b.cfg.Gated(),
		Pending: b.cfg.Pending(), Repeats: b.cfg.Repeats, ServerVersion: version,
		GeneratedAt: time.Now().UTC(), LLM: b.llm, PgSageVersion: sageVersion,
		PgSageCommit: sageCommit, MinRunsPerFamily: b.minRuns})
	if b.replay {
		report.AttachReplay(benchReplay(t, ctx, env, b.cfg, version))
	}
	t.Log("\n" + report.Markdown())
	jsonPath, mdPath, err := WriteReport(ReportDir(os.Getenv(EnvReportDir), t.TempDir()),
		report)
	if err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("report: %s, %s", jsonPath, mdPath)
	checkRuns(t, b.cfg, results)
	checkGates(t, b.cfg, benchGates(report))
	checkCoverage(t, b, expected, report.Gates, results)
}

// benchGates are the report's gates, then its replay section's (none
// without a replay).
func benchGates(r Report) []GateResult {
	out := append([]GateResult(nil), r.Gates...)
	if r.Replay != nil {
		out = append(out, r.Replay.Gates...)
	}
	return out
}

// benchPlan is what the environment asks the bench to run.
type benchPlan struct {
	cfg      RunConfig
	llm      LLMConfig
	selected []Scenario
	minRuns  int
	replay   bool
}

func planBench(t *testing.T) benchPlan {
	t.Helper()
	llm, err := LLMConfigFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	families, err := ParseFamilies(os.Getenv(EnvFamilies))
	if err != nil {
		t.Fatal(err)
	}
	selected := FilterScenarios(Scenarios(), families)
	repeats, minRuns, err := BenchRepeats(os.Getenv, selected)
	if err != nil {
		t.Fatal(err)
	}
	return benchPlan{cfg: DefaultConfig(repeats, llm), llm: llm, selected: selected,
		minRuns: minRuns, replay: BenchReplayRequested(os.Getenv)}
}

// budgetRepeats is the repeats the budget allows: a top-up may run up to
// MaxRepeats passes.
func (b benchPlan) budgetRepeats() int {
	if b.minRuns > 0 {
		return MaxRepeats
	}
	return b.cfg.Repeats
}

// scoredArms are the gated arms that are ready: the arms whose runs count
// toward the sample size.
func (b benchPlan) scoredArms() []string {
	pending := b.cfg.Pending()
	var out []string
	for _, a := range b.cfg.Gated() {
		if _, ok := pending[a]; !ok {
			out = append(out, a)
		}
	}
	return out
}

// run runs the selected scenarios, then tops up the short families.
func (b benchPlan) run(ctx context.Context, env *Env) []Result {
	results := Run(ctx, env, b.selected, b.cfg)
	if b.minRuns == 0 {
		return results
	}
	return topUpRuns(results, b.selected, b.scoredArms(), b.minRuns, b.cfg.Repeats,
		func(ss []Scenario, rep int) []Result {
			c := b.cfg
			c.Repeats, c.FirstRepeat = 1, rep
			return Run(ctx, env, ss, c)
		})
}

// benchExpected is the scenarios EnvExpected's manifest expects on the
// server's major; nil when EnvExpected is unset.
func benchExpected(t *testing.T, major int) map[string]bool {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(EnvExpected))
	if path == "" {
		return nil
	}
	m, err := LoadExpected(path, Scenarios())
	if err != nil {
		t.Fatal(err)
	}
	expected, err := m.For(major)
	if err != nil {
		t.Fatal(err)
	}
	return expected
}

// benchReplay replays the embedded corpus through the bench's live arms
// that can replay it, with the same model as the LLM-on arm.
func benchReplay(t *testing.T, ctx context.Context, env *Env, cfg RunConfig,
	version string) *ReplayReport {
	t.Helper()
	cases, err := replay.Corpus()
	if err != nil {
		t.Fatalf("replay corpus: %v", err)
	}
	var arms []LiveArm
	var names []string
	llm := LLMConfig{Mode: LLMFake}
	for _, a := range cfg.Live {
		if _, ok := a.(replayModeler); ok {
			arms, names = append(arms, a), append(names, a.Name())
		}
		if l, ok := a.(LLMArm); ok {
			llm = l.Config
		}
	}
	rep := BuildReplayReport(RunReplay(ctx, env, cases, arms), cases, ReplayMeta{
		Arms: names, Gated: names, LLM: llm, GeneratedAt: time.Now().UTC(),
		ServerVersion: version})
	return &rep
}

func checkRuns(t *testing.T, cfg RunConfig, results []Result) {
	t.Helper()
	live := map[string]bool{}
	for _, a := range cfg.Live {
		live[a.Name()] = true
	}
	for _, r := range results {
		if !live[r.Arm] {
			continue
		}
		if r.Attempts > 1 {
			t.Logf("%s %s ran %d times: the environment broke its premise", r.Arm,
				r.Scenario.ID, r.Attempts)
		}
		if r.Err != nil {
			t.Errorf("%s %s (repeat %d): %v", r.Arm, r.Scenario.ID, r.Repeat, r.Err)
		}
	}
}

func checkGates(t *testing.T, cfg RunConfig, gates []GateResult) {
	t.Helper()
	gated := map[string]bool{}
	for _, a := range cfg.Gated() {
		gated[a] = true
	}
	for _, g := range FailedGates(gates) {
		if gated[g.Arm] {
			t.Errorf("gate %s (%s, %s) failed: observed %s, threshold %s", g.ID, g.Family,
				g.Arm, g.Observed, g.Threshold)
		}
	}
	pending := cfg.Pending()
	for arm, why := range pending {
		t.Logf("arm %s not evaluated: %s", arm, why)
	}
	for _, g := range gates {
		if g.Status == GateNotEvaluated && gated[g.Arm] && pending[g.Arm] == "" {
			t.Logf("gate %s (%s, %s) not evaluated: %s", g.ID, g.Family, g.Arm, g.Reason)
		}
	}
}

// checkCoverage fails a release run whose coverage skips reduced: an
// expected scenario skipped, a gate skips cost a gated arm (with the
// manifest), or a family short of the bar (with a minimum).
func checkCoverage(t *testing.T, b benchPlan, expected map[string]bool,
	gates []GateResult, results []Result) {
	t.Helper()
	for _, msg := range ExpectedSkips(expected, results) {
		t.Errorf("%s (the manifest %s expects it scored)", msg, os.Getenv(EnvExpected))
	}
	for _, msg := range ShortFamilies(results, b.selected, b.scoredArms(), b.minRuns) {
		t.Errorf("%s=%d: %s", EnvMinRunsPerFamily, b.minRuns, msg)
	}
	if expected == nil {
		return
	}
	for _, g := range LostGates(gates, results, b.scoredArms()) {
		t.Errorf("gate %s (%s, %s) not evaluated: %s; skipped runs held its data: %s",
			g.ID, g.Family, g.Arm, g.Reason, skipReasons(results, g.Arm, g.Family))
	}
}

// skipReasons are the distinct skip reasons of arm's runs in family.
func skipReasons(results []Result, arm, family string) string {
	seen := map[string]bool{}
	var out []string
	for _, r := range results {
		if r.Arm == arm && string(r.Scenario.Family) == family && r.Skipped != "" &&
			!seen[r.Scenario.ID+r.Skipped] {
			seen[r.Scenario.ID+r.Skipped] = true
			out = append(out, r.Scenario.ID+": "+r.Skipped)
		}
	}
	return strings.Join(out, "; ")
}
