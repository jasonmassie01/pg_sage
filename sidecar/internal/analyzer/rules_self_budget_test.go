package analyzer

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/selfcost"
	"github.com/pg-sage/sidecar/internal/workload"
)

// Declared self-budget (roadmap phase 3): one sage_self_budget finding
// per database when pg_sage exceeds its own CPU, database-time, I/O or
// storage budget, naming the resources over budget and the top consumers
// (pg_sage's costliest statements and busiest loops) so the operator
// knows what to turn down.

func sampleBreaches() []selfbudget.Breach {
	return []selfbudget.Breach{
		{Resource: selfbudget.ResourceCPU, Used: 900, Limit: 600,
			Unit: "ms CPU per collector cycle"},
		{Resource: selfbudget.ResourceStorage, Used: 2 << 30, Limit: 1 << 30, Unit: "bytes"},
	}
}

func sampleTop() []selfcost.StatementCost {
	return []selfcost.StatementCost{
		{Text: "SELECT n.nspname FROM pg_class c", TimeMs: 420, Calls: 6, Blocks: 9000},
		{Text: "INSERT INTO sage.snapshots (category) VALUES ($1)", TimeMs: 80, Calls: 6},
	}
}

func sampleLoops() []selfbudget.LoopCost {
	return []selfbudget.LoopCost{{Name: "collector", BusyMs: 5200, Runs: 6},
		{Name: "analyzer", BusyMs: 1900, Runs: 6}}
}

func TestRuleSelfBudget_NoBreachNoFinding(t *testing.T) {
	if got := ruleSelfBudget("app", nil, sampleTop(), sampleLoops()); len(got) != 0 {
		t.Fatalf("findings = %+v, want none", got)
	}
}

func TestRuleSelfBudget_FindingNamesResourcesAndConsumers(t *testing.T) {
	got := ruleSelfBudget("app", sampleBreaches(), sampleTop(), sampleLoops())
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	f := got[0]
	if f.Category != categorySelfBudget || f.Severity != "warning" ||
		f.ObjectType != "database" || f.ObjectIdentifier != "app" {
		t.Fatalf("identity = %+v", f)
	}
	for _, want := range []string{"cpu", "storage", "app"} {
		if !strings.Contains(strings.ToLower(f.Title), want) {
			t.Errorf("title %q lacks %q", f.Title, want)
		}
	}
	breaches, ok := f.Detail["breaches"].([]map[string]any)
	if !ok || len(breaches) != 2 || breaches[0]["resource"] != "cpu" ||
		breaches[0]["used"] != 900.0 || breaches[0]["limit"] != 600.0 ||
		breaches[1]["resource"] != "storage" {
		t.Fatalf("breaches = %#v", f.Detail["breaches"])
	}
	stmts, ok := f.Detail["top_statements"].([]map[string]any)
	if !ok || len(stmts) != 2 || stmts[0]["text"] != "SELECT n.nspname FROM pg_class c" ||
		stmts[0]["db_time_ms"] != 420.0 || stmts[0]["blocks"] != int64(9000) {
		t.Fatalf("top statements = %#v", f.Detail["top_statements"])
	}
	loops, ok := f.Detail["top_loops"].([]map[string]any)
	if !ok || len(loops) != 2 || loops[0]["loop"] != "collector" ||
		loops[0]["busy_ms"] != 5200.0 || loops[0]["runs"] != int64(6) {
		t.Fatalf("top loops = %#v", f.Detail["top_loops"])
	}
	if f.Recommendation == "" || f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Fatalf("recommendation %q sql %q: want advice and nothing to run",
			f.Recommendation, f.RecommendedSQL)
	}
	if !strings.Contains(f.Recommendation, "collector") {
		t.Errorf("recommendation %q does not name the busiest loop", f.Recommendation)
	}
}

// The self-monitoring filter drops any finding that mentions pg_sage;
// this one is about pg_sage on purpose and must survive, so no field
// (statement texts included) may carry the name.
func TestRuleSelfBudget_SurvivesSelfMonitoringFilter(t *testing.T) {
	top := append(sampleTop(), selfcost.StatementCost{
		Text: "SET application_name = 'pg_sage'", TimeMs: 1, Calls: 1})
	f := ruleSelfBudget("app", sampleBreaches(), top, sampleLoops())[0]
	if isSelfMonitoringFinding(f) || excludedFromAdvice(f) ||
		workload.FindingExcluded(f.Detail) {
		t.Fatalf("self-budget finding would be dropped: %+v", f)
	}
	b, err := json.Marshal(f.Detail)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)+f.Title+f.Recommendation), "pg_sage") {
		t.Fatalf("finding mentions pg_sage: %s", b)
	}
}

func TestRuleSelfBudget_WithoutConsumers(t *testing.T) {
	f := ruleSelfBudget("app", sampleBreaches()[:1], nil, nil)[0]
	if s, _ := f.Detail["top_statements"].([]map[string]any); len(s) != 0 {
		t.Fatalf("top statements = %#v, want empty", f.Detail["top_statements"])
	}
	if f.Recommendation == "" {
		t.Fatal("recommendation must not depend on consumers being known")
	}
}

// fakeProcessCPU scripts the sidecar's CPU time.
type fakeProcessCPU struct {
	mu  sync.Mutex
	cpu time.Duration
	at  time.Time
}

func (f *fakeProcessCPU) read() (time.Duration, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cpu, true
}

func (f *fakeProcessCPU) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at
}

func (f *fakeProcessCPU) burn(wall, cpu time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.at, f.cpu = f.at.Add(wall), f.cpu+cpu
}

// Against PostgreSQL: two analyzer cycles; the second sees the CPU
// window, measures storage, and raises one sage_self_budget finding with
// both breaches. The metrics accessor reports the same usage.
func TestCheckSelfBudget_TwoCyclesAgainstPostgres(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 60
	cfg.Analyzer.SelfCostBudgetMs = 0
	cfg.SelfBudget.CPUMsPerCycle = 100
	cfg.SelfBudget.StorageMB = 0
	cfg.SelfBudget.BlocksPerHour = 0
	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	fake := &fakeProcessCPU{at: time.Now(), cpu: time.Minute}
	a.selfBudget.cpu = selfbudget.NewCPUMeter(fake.read, fake.now)
	a.eval = newCycleEval()
	if got := a.checkSelfCost(ctx); hasCategory(got, categorySelfBudget) {
		t.Fatalf("first cycle raised %+v without a CPU window", got)
	}
	if !a.eval.failed[categorySelfBudget] {
		t.Fatal("first cycle must leave the budget category unknown")
	}
	fake.burn(30*time.Second, 2*time.Second) // 4000 ms per 60 s cycle
	cfg.SelfBudget.StorageMB = 1             // any bootstrapped sage schema is above 1 MB
	a.eval = newCycleEval()
	got := a.checkSelfCost(ctx)
	f, ok := findCategory(got, categorySelfBudget)
	if !ok {
		t.Fatalf("second cycle findings = %+v, want %s", got, categorySelfBudget)
	}
	if !a.eval.ok[categorySelfBudget] {
		t.Fatal("second cycle did not evaluate the budget category")
	}
	breaches := f.Detail["breaches"].([]map[string]any)
	if len(breaches) != 2 || breaches[0]["resource"] != "cpu" || breaches[0]["used"] != 4000.0 ||
		breaches[1]["resource"] != "storage" {
		t.Fatalf("breaches = %#v, want cpu 4000 then storage", breaches)
	}
	u := a.SelfBudgetUsage()
	if !u.CPUKnown || u.CPUMsPerCycle != 4000 || !u.StorageKnown || u.StorageBytes <= 1<<20 {
		t.Fatalf("SelfBudgetUsage() = %+v", u)
	}
}

// Every resource disabled: still measured (metrics), category evaluated
// so an open finding resolves, never a finding.
func TestCheckSelfBudget_DisabledStillMeasures(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	cfg := phase2Config()
	cfg.Analyzer.SelfCostBudgetMs = 0
	cfg.SelfBudget = config.SelfBudgetConfig{}
	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	fake := &fakeProcessCPU{at: time.Now(), cpu: time.Minute}
	a.selfBudget.cpu = selfbudget.NewCPUMeter(fake.read, fake.now)
	a.eval = newCycleEval()
	a.checkSelfCost(ctx)
	fake.burn(time.Minute, time.Hour)
	a.eval = newCycleEval()
	if got := a.checkSelfCost(ctx); hasCategory(got, categorySelfBudget) {
		t.Fatalf("disabled budget raised %+v", got)
	}
	if !a.eval.ok[categorySelfBudget] {
		t.Fatal("disabled budget must evaluate the category")
	}
	if u := a.SelfBudgetUsage(); !u.CPUKnown || u.StorageBytes <= 0 {
		t.Fatalf("usage not measured with the budget disabled: %+v", u)
	}
}

// The database-time resource is only checked when self_budget sets it
// explicitly; inherited from analyzer.self_cost_budget_ms it stays the
// sage_self_cost finding's (one breach, one finding).
func TestSelfBudgetFor_DBTimeOnlyWhenExplicit(t *testing.T) {
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 60
	cfg.Analyzer.SelfCostBudgetMs = 3000
	cfg.SelfBudget.DBTimeMsPerHour = 0
	if b := selfBudgetFor(cfg); b.DBTimeMsPerHour != 0 {
		t.Fatalf("inherited DB time checked by self_budget: %+v", b)
	}
	cfg.SelfBudget.DBTimeMsPerHour = 90_000
	cfg.SelfBudget.StorageMB = 2
	b := selfBudgetFor(cfg)
	if b.DBTimeMsPerHour != 90_000 || b.StorageBytes != 2<<20 ||
		b.CPUMsPerCycle != float64(cfg.SelfBudget.CPUMsPerCycle) {
		t.Fatalf("budget = %+v", b)
	}
}

func TestCheckSelfBudget_ReadErrorKeepsCategoryUnknown(t *testing.T) {
	pool := phase2Pool(t)
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	a.checkSelfCost(canceled)
	if !a.eval.failed[categorySelfBudget] {
		t.Fatal("a failed reading must leave the budget category unknown")
	}
}

func hasCategory(ff []Finding, category string) bool {
	_, ok := findCategory(ff, category)
	return ok
}

func findCategory(ff []Finding, category string) (Finding, bool) {
	for _, f := range ff {
		if f.Category == category {
			return f, true
		}
	}
	return Finding{}, false
}
