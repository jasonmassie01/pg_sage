package srebench

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "sre-bench"))
}

// repeatBudget bounds one pass over the scenarios.
const repeatBudget = 8 * time.Minute

// TestPGIncidentBench runs every scenario's fault program on real
// PostgreSQL through each ready live arm (the causal graph with the LLM
// off; the LLM-on arm with the fake adversarial model or a live one),
// derives the always-escalate and rules-only baselines from the same
// evidence, and writes the JSON and Markdown report to
// SAGE_BENCH_REPORT_DIR (default: the test's temp dir). It fails when a
// fault program breaks or a live arm misses a pre-registered gate it is
// evaluated on; gates without the data to evaluate them are reported as
// not evaluated.
func TestPGIncidentBench(t *testing.T) {
	if !BenchRunRequested(os.Getenv) {
		t.Skipf("set %s=1 to run the full bench (CI runs it in its own step)", EnvRun)
	}
	dsn := testdb.SkipUnlessLive(t)
	repeats, err := ParseRepeats(os.Getenv(EnvRepeats))
	if err != nil {
		t.Fatal(err)
	}
	llm, err := LLMConfigFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig(repeats, llm)
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(repeats)*repeatBudget)
	t.Cleanup(cancel)
	env := NewEnv(ctx, t, dsn)
	var version string
	if err := env.Pool.QueryRow(ctx, "SELECT version()").Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	results := Run(ctx, env, Scenarios(), cfg)
	report := BuildReport(results, ReportMeta{Arms: cfg.ArmNames(), Gated: cfg.Gated(),
		Pending: cfg.Pending(), Repeats: repeats, ServerVersion: version,
		GeneratedAt: time.Now().UTC(), LLM: llm})
	report.Replay = benchReplay(t, ctx, env, cfg, version)
	t.Log("\n" + report.Markdown())
	jsonPath, mdPath, err := WriteReport(ReportDir(os.Getenv(EnvReportDir), t.TempDir()),
		report)
	if err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("report: %s, %s", jsonPath, mdPath)
	checkRuns(t, cfg, results)
	checkGates(t, cfg, append(append([]GateResult(nil), report.Gates...),
		report.Replay.Gates...))
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
