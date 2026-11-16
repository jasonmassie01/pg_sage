package advisor

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// advisors-02: open advisor findings are re-checked deterministically each
// cycle, one open finding gates only its own object, and the schedule is
// per sub-advisor (a run counts only on success).

type fakeOpen struct {
	findings []analyzer.Finding
	err      error
}

// OpenFindings only reads: the advisor calls it from parallel cycles
// (TestLifecycle_ParallelCyclesOneCall), as it does the real store.
func (f *fakeOpen) OpenFindings(context.Context, []string) ([]analyzer.Finding, error) {
	return f.findings, f.err
}

var lifecycleNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// lifecycleAdvisor is a WAL-only advisor (unless cfg says otherwise) on a
// fixed clock with the given open findings.
func lifecycleAdvisor(cfg *config.Config, mgr *llm.Manager, open *fakeOpen) *Advisor {
	a := New(nil, cfg, nil, mgr, noopLog)
	a.open = open
	a.hist = walFixtureHistory{}
	a.now = func() time.Time { return lifecycleNow }
	return a
}

func openWAL(sql string) analyzer.Finding {
	return analyzer.Finding{Category: "wal_tuning", Severity: "info",
		ObjectType: "configuration", ObjectIdentifier: "instance",
		Title: "wal_tuning recommendation for instance", Detail: map[string]any{},
		Recommendation: "raise it", RecommendedSQL: sql, ActionRisk: "moderate"}
}

func walSnapAt(maxWAL string) *collector.Snapshot {
	snap := walSnapshots()
	snap.ConfigData.PGSettings[0].Setting = maxWAL
	return snap
}

func findingIdents(fs []analyzer.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Category+"|"+f.ObjectIdentifier)
	}
	return out
}

// State transition: a WAL change the DBA applied by hand resolves.
func TestLifecycle_AppliedWALSettingResolves(t *testing.T) {
	mgr, capture := capturingManager(t, "[]")
	open := &fakeOpen{findings: []analyzer.Finding{
		openWAL("ALTER SYSTEM SET max_wal_size = '4GB';")}}
	a := lifecycleAdvisor(walOnlyConfig(""), mgr, open)
	got := a.analyzeSnapshot(context.Background(), walSnapAt("4096"))
	if slices.Contains(findingIdents(got), "wal_tuning|instance") {
		t.Fatalf("applied max_wal_size still emitted: %v", findingIdents(got))
	}
	if !slices.Contains(a.LastEvaluatedCategories(), "wal_tuning") {
		t.Fatalf("evaluated = %v, want wal_tuning so the open row resolves",
			a.LastEvaluatedCategories())
	}
	if len(capture()) != 1 {
		t.Fatalf("model calls = %d, want 1 (the rest of WAL is still reviewed)",
			len(capture()))
	}
}

// State transition: a setting not yet applied stays open, re-emitted
// unchanged, and is left out of the prompt.
func TestLifecycle_UnappliedWALSettingKeptAndGated(t *testing.T) {
	mgr, capture := capturingManager(t, "[]")
	f := openWAL("ALTER SYSTEM SET max_wal_size = '4GB';")
	a := lifecycleAdvisor(walOnlyConfig(""), mgr, &fakeOpen{findings: []analyzer.Finding{f}})
	got := a.analyzeSnapshot(context.Background(), walSnapAt("2048"))
	if len(got) != 1 || got[0].RecommendedSQL != f.RecommendedSQL ||
		got[0].Title != f.Title || got[0].ActionRisk != "moderate" {
		t.Fatalf("kept = %+v, want the open finding unchanged", got)
	}
	p := onlyPrompt(t, capture())
	if strings.Contains(p.user, "max_wal_size = 1024") ||
		strings.Contains(p.user, "max_wal_size = 2048") {
		t.Fatalf("gated max_wal_size still in the prompt:\n%s", p.user)
	}
	if !strings.Contains(p.user, "checkpoint_timeout = 300s") {
		t.Fatalf("ungated checkpoint_timeout missing:\n%s", p.user)
	}
	if !strings.Contains(p.user, "do not recommend them): max_wal_size") {
		t.Fatalf("prompt does not name the left-out setting:\n%s", p.user)
	}
}

// The model proposing a gated setting anyway is dropped; an ungated one
// is kept (the deterministic gate after every call stays).
func TestLifecycle_GatedProposalDropped(t *testing.T) {
	mgr, _ := capturingManager(t, `[{"object_identifier":"instance","rationale":"r",`+
		`"recommended_sql":"ALTER SYSTEM SET max_wal_size = '8GB'; `+
		`ALTER SYSTEM SET checkpoint_timeout = '15min'"}]`)
	f := openWAL("ALTER SYSTEM SET max_wal_size = '4GB';")
	a := lifecycleAdvisor(walOnlyConfig(""), mgr, &fakeOpen{findings: []analyzer.Finding{f}})
	got := findingIdents(a.analyzeSnapshot(context.Background(), walSnapAt("2048")))
	want := []string{"wal_tuning|instance", "wal_tuning|instance:checkpoint_timeout"}
	if !slices.Equal(got, want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
}

// Bloat: an open finding on t1 keeps t1 out of the prompt; newly bloated
// t2 is still asked about.
func TestLifecycle_BloatGatePerTable(t *testing.T) {
	mgr, capture := capturingManager(t, "[]")
	cfg := walOnlyConfig("")
	cfg.Advisor.WALEnabled, cfg.Advisor.BloatEnabled = false, true
	open := &fakeOpen{findings: []analyzer.Finding{{Category: "bloat_remediation",
		Severity: "info", ObjectIdentifier: "public.t1", Title: "bloat t1",
		Detail: map[string]any{}}}}
	a := lifecycleAdvisor(cfg, mgr, open)
	got := a.analyzeSnapshot(context.Background(), deadTupleSnapshot("t1", "t2"))
	p := onlyPrompt(t, capture())
	if !strings.Contains(p.user, "Table: public.t2") {
		t.Fatalf("prompt lacks the newly bloated t2:\n%s", p.user)
	}
	if strings.Contains(p.user, "Table: public.t1") {
		t.Fatalf("prompt names t1, which has an open finding:\n%s", p.user)
	}
	if !slices.Equal(findingIdents(got), []string{"bloat_remediation|public.t1"}) {
		t.Fatalf("findings = %v, want t1 kept open", findingIdents(got))
	}
}

// State transition: LLM off reports advisor_degraded; on again, it is an
// evaluated category with no finding, so it resolves.
func TestLifecycle_DegradedResolvesWhenLLMReturns(t *testing.T) {
	mgr, _ := capturingManager(t, "[]")
	cfg := walOnlyConfig("")
	cfg.LLM.Enabled = false
	a := lifecycleAdvisor(cfg, mgr, &fakeOpen{})
	got := a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	if !slices.Contains(findingIdents(got), "advisor_degraded|llm") {
		t.Fatalf("LLM off: findings = %v, want advisor_degraded", findingIdents(got))
	}
	if slices.Contains(a.LastEvaluatedCategories(), "advisor_degraded") {
		t.Fatal("LLM off: advisor_degraded must not be evaluated-and-absent")
	}
	cfg.LLM.Enabled = true
	got = a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	if slices.Contains(findingIdents(got), "advisor_degraded|llm") {
		t.Fatalf("LLM on: advisor_degraded still emitted")
	}
	if !slices.Contains(a.LastEvaluatedCategories(), "advisor_degraded") {
		t.Fatalf("LLM on: evaluated = %v, want advisor_degraded",
			a.LastEvaluatedCategories())
	}
}

// Error propagation: a model error leaves the last success unchanged,
// backs off 1h (then 2h, capped at the interval) and leaves the category
// unevaluated.
func TestLifecycle_LLMErrorBacksOff(t *testing.T) {
	srv, mgr := mockLLMServerError(t)
	defer srv.Close()
	cfg := walOnlyConfig("")
	cfg.Advisor.IntervalSeconds = 3 * 3600
	a := lifecycleAdvisor(cfg, mgr, &fakeOpen{})
	a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	st := a.schedule.get("wal")
	if !st.LastSuccessAt.IsZero() || st.ConsecutiveFailures != 1 ||
		!st.NextAttemptAt.Equal(lifecycleNow.Add(time.Hour)) {
		t.Fatalf("after one failure: %+v, want no success, 1 failure, next +1h", st)
	}
	if slices.Contains(a.LastEvaluatedCategories(), "wal_tuning") {
		t.Fatalf("evaluated = %v: a failed sub-advisor's category was evaluated",
			a.LastEvaluatedCategories())
	}
	for i, want := range []time.Duration{2 * time.Hour, 3 * time.Hour} {
		lifecycleClock(a, st.NextAttemptAt)
		a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
		st = a.schedule.get("wal")
		if st.ConsecutiveFailures != i+2 || !st.NextAttemptAt.Equal(a.now().Add(want)) {
			t.Fatalf("failure %d: %+v, want next +%v", i+2, st, want)
		}
	}
}

func lifecycleClock(a *Advisor, at time.Time) { a.now = func() time.Time { return at } }

// Budget errors count as failures.
func TestRecord_BudgetErrorIsFailure(t *testing.T) {
	a := lifecycleAdvisor(walOnlyConfig(""), nil, &fakeOpen{})
	a.record(context.Background(), "wal", lifecycleNow,
		errors.New("wal LLM: daily token budget exhausted"))
	st := a.schedule.get("wal")
	if st.ConsecutiveFailures != 1 || !st.NextAttemptAt.Equal(lifecycleNow.Add(time.Hour)) {
		t.Fatalf("budget error: %+v, want a 1h back-off", st)
	}
	saved, _ := a.runs.LoadRuns(context.Background())
	if saved["wal"] != st {
		t.Fatalf("saved %+v, want %+v", saved["wal"], st)
	}
}

// Error propagation: unreadable open findings skip every sub-advisor
// (fail closed on spend) and evaluate no advisor category.
func TestLifecycle_OpenFindingsErrorSkipsSubAdvisors(t *testing.T) {
	mgr, capture := capturingManager(t, walAdvice)
	open := &fakeOpen{err: errors.New("connection refused")}
	a := lifecycleAdvisor(walOnlyConfig(""), mgr, open)
	got := a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	if len(capture()) != 0 || len(got) != 0 {
		t.Fatalf("calls = %d, findings = %v; want none", len(capture()), got)
	}
	if ev := a.LastEvaluatedCategories(); !slices.Equal(ev, []string{"advisor_degraded"}) {
		t.Fatalf("evaluated = %v, want only advisor_degraded", ev)
	}
	if !a.schedule.get("wal").due(lifecycleNow) {
		t.Fatal("a skipped cycle must not count as a run")
	}
}

type failingRuns struct{ err error }

func (f failingRuns) LoadRuns(context.Context) (map[string]runState, error) {
	return nil, f.err
}
func (f failingRuns) ClaimRun(context.Context, string, time.Time, time.Time) (bool, error) {
	return false, f.err
}
func (f failingRuns) SaveRun(context.Context, string, runState) error { return f.err }

// Error propagation: a schedule that cannot be claimed makes no call.
func TestLifecycle_ClaimErrorMakesNoCall(t *testing.T) {
	mgr, capture := capturingManager(t, walAdvice)
	a := lifecycleAdvisor(walOnlyConfig(""), mgr, &fakeOpen{})
	a.runs = failingRuns{errors.New("relation sage.advisor_runs does not exist")}
	a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	if n := len(capture()); n != 0 {
		t.Fatalf("model calls = %d, want 0 when the claim fails", n)
	}
}

// Boundary: the interval elapsing exactly makes the sub-advisor due; one
// nanosecond earlier it is not.
func TestLifecycle_IntervalExactlyElapsed(t *testing.T) {
	mgr, capture := capturingManager(t, "[]")
	cfg := walOnlyConfig("")
	cfg.Advisor.IntervalSeconds = 3600
	a := lifecycleAdvisor(cfg, mgr, &fakeOpen{})
	a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	lifecycleClock(a, lifecycleNow.Add(time.Hour-time.Nanosecond))
	a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	if n := len(capture()); n != 1 {
		t.Fatalf("calls before the interval = %d, want 1", n)
	}
	lifecycleClock(a, lifecycleNow.Add(time.Hour))
	a.analyzeSnapshot(context.Background(), walSnapAt("1024"))
	if n := len(capture()); n != 2 {
		t.Fatalf("calls at exactly the interval = %d, want 2", n)
	}
	if st := a.schedule.get("wal"); !st.LastSuccessAt.Equal(lifecycleNow.Add(time.Hour)) {
		t.Fatalf("last success = %v, want the second run", st.LastSuccessAt)
	}
}

// Nil/empty: no pool and no config snapshot -- nothing runs, nothing
// resolves, and only the degraded warning (LLM on) is evaluated.
func TestLifecycle_NilPoolNoSnapshot(t *testing.T) {
	mgr, capture := capturingManager(t, walAdvice)
	a := New(nil, walOnlyConfig(""), nil, mgr, noopLog)
	if _, ok := a.open.(noOpenFindings); !ok {
		t.Fatalf("nil pool: open store = %T, want noOpenFindings", a.open)
	}
	for _, snap := range []*collector.Snapshot{nil, {}} {
		if got := a.analyzeSnapshot(context.Background(), snap); len(got) != 0 {
			t.Fatalf("snapshot %+v: findings = %v, want none", snap, got)
		}
		if ev := a.LastEvaluatedCategories(); !slices.Equal(ev, []string{"advisor_degraded"}) {
			t.Fatalf("evaluated = %v, want only advisor_degraded", ev)
		}
	}
	if len(capture()) != 0 {
		t.Fatal("no snapshot, yet the model was called")
	}
	got, err := a.Analyze(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("Analyze without a collector = %v, %v; want none", got, err)
	}
}

// Concurrency: two cycles in parallel -- on one advisor, and on two
// advisors sharing a schedule store (two replicas) -- make one model call.
func TestLifecycle_ParallelCyclesOneCall(t *testing.T) {
	mgr, capture := capturingManager(t, "[]")
	shared := newMemRunStore()
	a := lifecycleAdvisor(walOnlyConfig(""), mgr, &fakeOpen{})
	b := lifecycleAdvisor(walOnlyConfig(""), mgr, &fakeOpen{})
	a.runs, b.runs = shared, shared
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, adv := range []*Advisor{a, a, b, b} {
		wg.Add(1)
		go func(adv *Advisor) {
			defer wg.Done()
			<-start
			adv.analyzeSnapshot(context.Background(), walSnapAt("1024"))
		}(adv)
	}
	close(start)
	wg.Wait()
	if n := len(capture()); n != 1 {
		t.Fatalf("model calls = %d, want 1 for four parallel cycles", n)
	}
}
