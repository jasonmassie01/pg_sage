package srebench

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// Roadmap 2.4: the nightly live-model arm replays the corpus through the
// causal graph and the LLM-on arm against a live model, within hard caps,
// and writes a PGIncidentBench report (schema revision 2) carrying the
// held-out model lift, which the release workflow signs like the release
// bench. It runs only when SAGE_BENCH_LIVE_ARM=1 and the live model is
// configured (PG_SAGE_LIVE_LLM=1 and every cap); it fails closed when a
// cap is reached or a safety gate fails. Quality gates are measured and
// reported, never a reason to fail the nightly.

func TestLiveArmRequested(t *testing.T) {
	for v, want := range map[string]bool{"1": true, " 1 ": true, "": false, "0": false,
		"true": false} {
		if got := LiveArmRequested(env(map[string]string{EnvLiveArm: v})); got != want {
			t.Errorf("%s=%q: %v, want %v", EnvLiveArm, v, got, want)
		}
	}
}

func liveReport(t *testing.T, budget *Budget) Report {
	t.Helper()
	cfg := LLMConfig{Mode: LLMLive, Model: "gpt-4o-mini", budget: budget}
	cases := []replay.Case{{ID: "c1", Family: "lock_blocking", Class: replay.ClassPositive}}
	return BuildLiveReport(overrideMix(), cases, LiveMeta{LLM: cfg,
		GeneratedAt: time.Now().UTC().Add(-time.Minute), ServerVersion: "PostgreSQL 17",
		PgSageVersion: "1.9.1", PgSageCommit: strings.Repeat("a", 40)})
}

func TestBuildLiveReport_IsAnIngestibleLiftReport(t *testing.T) {
	b := NewBudget(caps(), time.Now)
	r := liveReport(t, b)
	if r.Replay == nil || len(r.ModelLift) == 0 || r.LLMBudget == nil ||
		r.SchemaRevision != 2 || r.PgSageCommit != strings.Repeat("a", 40) {
		t.Fatalf("live report = %+v", r)
	}
	for _, c := range r.Cells {
		if c.Family != PooledFamily || c.Runs != 0 {
			t.Fatalf("a replay-only report must carry no family cell: %+v", c)
		}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	run, err := earned.ParseBenchReport(raw, time.Now())
	if err != nil || run.ModelLift == nil || run.ModelLift.BudgetExhausted ||
		len(earned.SummarizeBench(&run).Families) != 0 {
		t.Fatalf("ingested live report = %+v (%v)", run.ModelLift, err)
	}
}

func TestCheckLiveReport(t *testing.T) {
	ok := liveReport(t, NewBudget(caps(), time.Now))
	if fails, _ := CheckLiveReport(ok); len(fails) != 0 {
		t.Fatalf("a clean live report failed: %v", fails)
	}
	spent := NewBudget(BudgetCaps{MaxRequests: 1, MaxTokens: 1e6, MaxWall: time.Hour,
		MaxSpendUSD: 1}, time.Now)
	res, _ := spent.Admit(10, 10)
	spent.Settle(res, http.StatusOK, TapUsage{PromptTokens: 5})
	_, _ = spent.Admit(10, 10)
	fails, _ := CheckLiveReport(liveReport(t, spent))
	if len(fails) == 0 || !strings.Contains(strings.Join(fails, " "), "budget") {
		t.Fatalf("an exhausted budget must fail the run: %v", fails)
	}
	unsafe := liveReport(t, NewBudget(caps(), time.Now))
	unsafe.Replay.Gates = append(unsafe.Replay.Gates, GateResult{ID: GateForbidden,
		Family: lockFam, Arm: ArmLLM, Status: GateFail, Split: SplitAll})
	if fails, _ := CheckLiveReport(unsafe); len(fails) != 1 ||
		!strings.Contains(fails[0], GateForbidden) {
		t.Fatalf("a failed safety gate must fail the run: %v", fails)
	}
	quality := liveReport(t, NewBudget(caps(), time.Now))
	quality.Replay.Gates = append(quality.Replay.Gates, GateResult{ID: GateTop1,
		Family: lockFam, Arm: ArmLLM, Status: GateFail, Split: replay.SplitHeldOut})
	fails, notes := CheckLiveReport(quality)
	if len(fails) != 0 || len(notes) == 0 || !strings.Contains(notes[0], GateTop1) {
		t.Fatalf("a quality gate is measured, not a failure: fails %v notes %v", fails, notes)
	}
	noReplay := liveReport(t, NewBudget(caps(), time.Now))
	noReplay.AttachReplay(nil)
	if fails, _ := CheckLiveReport(noReplay); len(fails) == 0 {
		t.Fatal("a live report without the replay measured nothing")
	}
}

// TestLiveModelArm is the nightly live-model arm. It never runs in pull
// request CI or a plain test run.
func TestLiveModelArm(t *testing.T) {
	if !LiveArmRequested(os.Getenv) {
		t.Skipf("set %s=1 (with %s=1 and the caps) to run the nightly live-model arm",
			EnvLiveArm, EnvLiveLLM)
	}
	llm, err := LLMConfigFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if llm.Mode != LLMLive {
		t.Fatalf("%s=1 needs a live model (%s); refusing to measure the fake", EnvLiveArm,
			EnvLLMURL)
	}
	split, err := ParseSplit(os.Getenv(EnvSplit))
	if err != nil {
		t.Fatal(err)
	}
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), llm.Caps.MaxWall+replayBudget)
	t.Cleanup(cancel)
	all, err := replay.Corpus()
	if err != nil {
		t.Fatalf("corpus: %v", err)
	}
	cases, err := replay.FilterSplit(all, split)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEnv(ctx, t, dsn)
	var version string
	if err := e.Pool.QueryRow(ctx, "SELECT version()").Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	rs := RunReplay(ctx, e, cases, []LiveArm{CausalGraph{}, LLMArm{Config: llm}})
	sageVersion, sageCommit := BuildFromEnv(os.Getenv)
	report := BuildLiveReport(rs, cases, LiveMeta{LLM: llm, GeneratedAt: time.Now().UTC(),
		ServerVersion: version, PgSageVersion: sageVersion, PgSageCommit: sageCommit})
	t.Log("\n" + report.Markdown())
	jsonPath, mdPath, err := WriteReport(ReportDir(os.Getenv(EnvReportDir), t.TempDir()),
		report)
	if err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("live report: %s, %s", jsonPath, mdPath)
	fails, notes := CheckLiveReport(report)
	for _, n := range notes {
		t.Log(n)
	}
	for _, f := range fails {
		t.Error(f)
	}
}
