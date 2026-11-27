package analyzer

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/workload"
)

// footprint-07: self_budget.cpu_ms_per_cycle is a process budget. The
// analyzers no longer check it per database; the process watch raises it
// once, as one sage_self_budget finding on the "sidecar" object, which the
// analyzer of the database that carries it keeps open.

func cpuBreach() selfbudget.Breach {
	return selfbudget.Breach{Resource: selfbudget.ResourceCPU, Used: 900, Limit: 600,
		Unit: "ms CPU per collector cycle"}
}

func TestSelfBudgetFor_NeverChecksCPU(t *testing.T) {
	cfg := phase2Config()
	cfg.SelfBudget = config.SelfBudgetConfig{CPUMsPerCycle: 1, StorageMB: 3,
		BlocksPerHour: 7}
	b := selfBudgetFor(cfg)
	if b.CPUMsPerCycle != 0 || b.StorageBytes != 3<<20 || b.BlocksPerHour != 7 {
		t.Fatalf("budget = %+v, want storage and blocks without CPU", b)
	}
}

// Unknown CPU never makes a database's budget undecidable.
func TestBudgetDecidable_WithoutCPU(t *testing.T) {
	if !budgetDecidable(selfbudget.Budget{}, selfbudget.Usage{}) {
		t.Fatal("nothing enabled must be decidable")
	}
	if budgetDecidable(selfbudget.Budget{StorageBytes: 1}, selfbudget.Usage{}) {
		t.Fatal("storage enabled but unmeasured must stay unknown")
	}
	if budgetDecidable(selfbudget.Budget{BlocksPerHour: 1}, selfbudget.Usage{}) {
		t.Fatal("blocks enabled but unmeasured must stay unknown")
	}
}

func TestSidecarCPUFinding_Identity(t *testing.T) {
	loops := []selfbudget.LoopCost{{Name: "collector:db2", BusyMs: 5200, Runs: 6},
		{Name: "notify", BusyMs: 100, Runs: 1}}
	f := SidecarCPUFinding(cpuBreach(), loops)
	if f.Category != categorySelfBudget || f.ObjectType != SidecarObject ||
		f.ObjectIdentifier != SidecarObject || f.Severity != "warning" {
		t.Fatalf("identity = %+v, want sage_self_budget on sidecar/sidecar", f)
	}
	if !strings.Contains(f.Title, "pg_sage") || !strings.Contains(f.Title, "cpu") ||
		!strings.Contains(f.Title, "process") {
		t.Fatalf("title %q must name pg_sage's process cpu", f.Title)
	}
	rows := f.Detail["breaches"].([]map[string]any)
	if len(rows) != 1 || rows[0]["resource"] != "cpu" || rows[0]["used"] != 900.0 ||
		rows[0]["limit"] != 600.0 || rows[0]["share"] != 1.5 {
		t.Fatalf("breaches = %#v", rows)
	}
	top := f.Detail["top_loops"].([]map[string]any)
	if len(top) != 2 || top[0]["loop"] != "collector:db2" || top[0]["busy_ms"] != 5200.0 {
		t.Fatalf("top loops = %#v", top)
	}
	if s, ok := f.Detail["top_statements"].([]map[string]any); !ok || len(s) != 0 {
		t.Fatalf("top statements = %#v, want empty: CPU is not a statement cost",
			f.Detail["top_statements"])
	}
	if !strings.Contains(f.Recommendation, "collector:db2") || f.RecommendedSQL != "" {
		t.Fatalf("recommendation %q sql %q", f.Recommendation, f.RecommendedSQL)
	}
	if isSelfMonitoringFinding(f) || excludedFromAdvice(f) ||
		workload.FindingExcluded(f.Detail) {
		t.Fatalf("sidecar finding would be dropped: %+v", f)
	}
}

func TestSidecarCPUFinding_WithoutLoops(t *testing.T) {
	f := SidecarCPUFinding(cpuBreach(), nil)
	if l, ok := f.Detail["top_loops"].([]map[string]any); !ok || len(l) != 0 {
		t.Fatalf("top loops = %#v, want empty", f.Detail["top_loops"])
	}
	if f.Recommendation == "" {
		t.Fatal("recommendation must not depend on loops being known")
	}
}

// Concurrency on PostgreSQL: three analyzers check the budget at once
// with cpu_ms_per_cycle 1. None raises a CPU breach; each keeps the
// sidecar identity open for the process watch.
func TestCheckSelfBudget_ConcurrentAnalyzersRaiseNoCPU(t *testing.T) {
	pool := phase2Pool(t)
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 60
	cfg.Analyzer.SelfCostBudgetMs = 0
	cfg.SelfBudget = config.SelfBudgetConfig{CPUMsPerCycle: 1, StorageMB: 1}
	var wg sync.WaitGroup
	results := make([][]Finding, 3)
	kept := make([]bool, 3)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := New(pool, cfg, startedCollector(cfg), nil, nil, nil, nil, noopLog)
			for c := 0; c < 2; c++ {
				a.eval = newCycleEval()
				results[i] = a.checkSelfCost(context.Background())
			}
			kept[i] = a.eval.kept(categorySelfBudget)[SidecarObject]
		}(i)
	}
	wg.Wait()
	for i, got := range results {
		f, ok := findCategory(got, categorySelfBudget)
		if !ok {
			t.Fatalf("analyzer %d: no storage finding: %+v", i, got)
		}
		for _, b := range f.Detail["breaches"].([]map[string]any) {
			if b["resource"] == "cpu" {
				t.Fatalf("analyzer %d raised a database CPU breach: %#v", i, b)
			}
		}
		if !kept[i] {
			t.Fatalf("analyzer %d does not keep the sidecar identity open", i)
		}
	}
}

func clearSidecarFindings(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	clean := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.findings
			WHERE category = 'sage_self_budget'
			  AND object_identifier IN ('sidecar', 'footprint07_db')`)
	}
	clean()
	t.Cleanup(clean)
}

func openSelfBudgetIdents(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT object_identifier, count(*)
		FROM sage.findings WHERE category = 'sage_self_budget' AND status = 'open'
		  AND object_identifier IN ('sidecar', 'footprint07_db')
		GROUP BY object_identifier`)
	if err != nil {
		t.Fatalf("read open findings: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var ident string
		var n int
		if err := rows.Scan(&ident, &n); err != nil {
			t.Fatal(err)
		}
		out[ident] = n
	}
	return out
}

// Integration: ResolveSidecarSelfBudget resolves only the sidecar
// finding, once; a second call finds nothing and is not an error.
func TestResolveSidecarSelfBudget_OnlyTheSidecarFinding(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	clearSidecarFindings(t, pool)
	dbFinding := ruleSelfBudget("footprint07_db", sampleBreaches()[1:], nil, nil)
	if err := UpsertFindings(ctx, pool, append(dbFinding,
		SidecarCPUFinding(cpuBreach(), nil))); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got := openSelfBudgetIdents(t, pool); got["sidecar"] != 1 || got["footprint07_db"] != 1 {
		t.Fatalf("open = %v, want one of each", got)
	}
	n, err := ResolveSidecarSelfBudget(ctx, pool)
	if err != nil || n != 1 {
		t.Fatalf("resolve = %d, %v; want 1 resolved", n, err)
	}
	if got := openSelfBudgetIdents(t, pool); got["sidecar"] != 0 || got["footprint07_db"] != 1 {
		t.Fatalf("open = %v, want only the database finding", got)
	}
	if n, err := ResolveSidecarSelfBudget(ctx, pool); err != nil || n != 0 {
		t.Fatalf("second resolve = %d, %v; want 0 and no error", n, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ResolveSidecarSelfBudget(canceled, pool); err == nil ||
		!strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("cancelled resolve err = %v, want an error naming the sidecar finding", err)
	}
}

// State transition on PostgreSQL: an analyzer cycle that evaluates the
// category resolves this database's cleared finding but keeps the
// process watch's sidecar finding open.
func TestResolveCleared_KeepsTheSidecarFinding(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	clearSidecarFindings(t, pool)
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 60
	cfg.Analyzer.SelfCostBudgetMs = 0
	cfg.SelfBudget = config.SelfBudgetConfig{}
	dbFinding := ruleSelfBudget("footprint07_db", sampleBreaches()[1:], nil, nil)
	if err := UpsertFindings(ctx, pool, append(dbFinding,
		SidecarCPUFinding(cpuBreach(), nil))); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	a := New(pool, cfg, startedCollector(cfg), nil, nil, nil, nil, noopLog)
	a.eval = newCycleEval()
	findings := a.checkSelfCost(ctx)
	a.resolveCleared(ctx, findings, a.eval.resolvable(findings))
	if got := openSelfBudgetIdents(t, pool); got["sidecar"] != 1 || got["footprint07_db"] != 0 {
		t.Fatalf("open = %v, want the sidecar finding kept and the database one resolved",
			got)
	}
}
