package analyzer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/selfcost"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

func knownCost(dbMs float64) selfcost.Cost {
	return selfcost.Cost{Known: true, DBTimeKnown: true, Database: "app",
		DBTimeMsPerCycle: dbMs, CallsPerCycle: 40, BlocksPerCycle: 900,
		RowsReadPerCycle: 1200, RowsWrittenPerCycle: 35, SchemaBytes: 1 << 20,
		WindowSeconds: 600, CycleSeconds: 60}
}

func TestRuleSelfCost_OverBudgetRaisesWarning(t *testing.T) {
	got := ruleSelfCost(knownCost(4200), 3000)
	if len(got) != 1 {
		t.Fatalf("findings = %d, want 1", len(got))
	}
	f := got[0]
	if f.Category != categorySelfCost || f.Severity != "warning" ||
		f.ObjectType != "database" || f.ObjectIdentifier != "app" {
		t.Fatalf("finding identity = %+v", f)
	}
	if !strings.Contains(f.Title, "4200 ms") || !strings.Contains(f.Title, "3000 ms") {
		t.Fatalf("title = %q, want measured and budget", f.Title)
	}
	for key, want := range map[string]any{
		"db_time_ms_per_cycle": 4200.0, "budget_ms": 3000, "cycle_seconds": 60.0,
		"window_seconds": 600.0, "calls_per_cycle": 40.0, "blocks_per_cycle": 900.0,
		"rows_read_per_cycle": 1200.0, "rows_written_per_cycle": 35.0,
		"schema_bytes": int64(1 << 20),
	} {
		if f.Detail[key] != want {
			t.Errorf("detail[%s] = %#v, want %#v", key, f.Detail[key], want)
		}
	}
	if f.Recommendation == "" || f.RecommendedSQL != "" {
		t.Fatalf("recommendation = %q / sql = %q, want advice and no SQL to run",
			f.Recommendation, f.RecommendedSQL)
	}
}

// The finding is about pg_sage's own cost, but it must not be swallowed by
// the self-monitoring filter that drops findings about pg_sage's queries.
func TestRuleSelfCost_FindingSurvivesSelfMonitorFilter(t *testing.T) {
	f := ruleSelfCost(knownCost(9000), 3000)[0]
	if isSelfMonitoringFinding(f) {
		t.Fatalf("self-cost finding is dropped as self-monitoring: %+v", f)
	}
	if selfmonitor.IsFinding(selfmonitor.FindingFields{ObjectIdentifier: f.ObjectIdentifier,
		Title: f.Title, Detail: f.Detail}) {
		t.Fatal("self-cost finding matches the list API's self-monitoring exclusion")
	}
}

func TestRuleSelfCost_NoFinding(t *testing.T) {
	cases := map[string]struct {
		cost   selfcost.Cost
		budget int
	}{
		"exactly at budget": {knownCost(3000), 3000},
		"under budget":      {knownCost(10), 3000},
		"budget disabled":   {knownCost(1e9), 0},
		"unknown window":    {selfcost.Cost{DBTimeKnown: true, DBTimeMsPerCycle: 1e9}, 1},
		"unknown DB time":   {selfcost.Cost{Known: true, DBTimeMsPerCycle: 1e9}, 1},
		"zero-value cost":   {selfcost.Cost{}, 3000},
	}
	for name, tc := range cases {
		if got := ruleSelfCost(tc.cost, tc.budget); len(got) != 0 {
			t.Errorf("%s: findings = %+v, want none", name, got)
		}
	}
}

// Against PostgreSQL: the analyzer measures its own database once per
// cycle; the first cycle has no window (the category stays unknown so an
// open finding is not resolved by a restart), the next one raises the
// finding when pg_sage's time per collector cycle is over budget.
func TestCheckSelfCost_TwoCyclesAgainstPostgres(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	var preloaded bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension
		WHERE extname='pg_stat_statements')`).Scan(&preloaded)
	if !preloaded {
		t.Skip("pg_stat_statements not installed")
	}
	cfg := phase2Config()
	cfg.Analyzer.SelfCostBudgetMs = 1
	cfg.Collector.IntervalSeconds = 60
	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	if got := a.checkSelfCost(ctx); len(got) != 0 {
		t.Fatalf("first cycle raised %+v without a window", got)
	}
	if !a.eval.failed[categorySelfCost] {
		t.Fatal("first cycle must leave the category unknown, not evaluated")
	}
	if _, err := sagePool(t).Exec(ctx,
		"SELECT pg_sleep(0.05) AS analyzer_selfcost_probe"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	a.eval = newCycleEval()
	got := a.checkSelfCost(ctx)
	if len(got) != 1 || got[0].Category != categorySelfCost {
		t.Fatalf("second cycle findings = %+v, want one %s", got, categorySelfCost)
	}
	if !a.eval.ok[categorySelfCost] {
		t.Fatal("second cycle did not mark the category evaluated")
	}
	if c := a.SelfCost(); !c.Known || !c.DBTimeKnown || c.DBTimeMsPerCycle <= 1 {
		t.Fatalf("SelfCost() = %+v, want the measured window", c)
	}
}

func TestCheckSelfCost_DisabledBudgetStillMeasures(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	cfg := phase2Config()
	cfg.Analyzer.SelfCostBudgetMs = 0
	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	if got := a.checkSelfCost(ctx); len(got) != 0 {
		t.Fatalf("disabled budget raised %+v", got)
	}
	if !a.eval.ok[categorySelfCost] {
		t.Fatal("disabled budget must evaluate the category so an open finding resolves")
	}
	if a.SelfCost().SchemaBytes <= 0 {
		t.Fatalf("metrics not measured with the budget disabled: %+v", a.SelfCost())
	}
}

func TestCheckSelfCost_ReadErrorKeepsCategoryUnknown(t *testing.T) {
	pool := phase2Pool(t)
	cfg := phase2Config()
	cfg.Analyzer.SelfCostBudgetMs = 3000
	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := a.checkSelfCost(canceled); len(got) != 0 {
		t.Fatalf("failed read raised %+v", got)
	}
	if !a.eval.failed[categorySelfCost] {
		t.Fatal("a failed read must leave the category unknown")
	}
}
