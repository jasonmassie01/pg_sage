package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// vacuum-08: one freeze custodian per database runtime, whose sampler the
// analyzer and the runway advisor share; one red table yields exactly one
// VACUUM (FREEZE) across the three.

func wraparoundRule(t *testing.T) analyzer.RuleFunc {
	t.Helper()
	for _, rule := range analyzer.AllRules {
		if rule.Name == "wraparound_freeze" {
			return rule.Fn
		}
	}
	t.Fatal("wraparound_freeze rule is not registered")
	return nil
}

// redReloptionTable creates table frozen, with a 100,000 freeze maximum,
// and burns 80,000 XIDs: the table is red, every other one green.
func redReloptionTable(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	ctx := context.Background()
	for _, sql := range []string{"DROP TABLE IF EXISTS " + table,
		"CREATE TABLE " + table + ` (id int) WITH (autovacuum_enabled = false,
			autovacuum_freeze_max_age = 100000)`,
		"INSERT INTO " + table + " VALUES (1)", "VACUUM (FREEZE) " + table} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	testdb.BurnXIDs(t, ctx, pool, 80000)
}

// adviseWraparound runs the runway advisor over custodian for table.
func adviseWraparound(t *testing.T, custodian autonomy.Custodian,
	table string) []sre.ActionProposal {
	t.Helper()
	advisor := newRunwayAdvisor(custodian, &scanStub{}, func(context.Context) bool { return false })
	exec := executor.New(nil, &config.Config{}, time.Time{}, func(string, string, ...any) {})
	exec.WithPolicyGate(&explainStub{verdict: policy.Decision{
		Verdict: policy.VerdictQueueApproval, RiskTier: policy.RiskSafe}})
	advisor.attach(exec)
	advised, err := advisor.Advise(context.Background(), sre.AdviceRequest{
		Kind: sre.TriggerWraparound, Subject: "table " + table})
	if err != nil {
		t.Fatalf("advisor: %v", err)
	}
	return advised
}

// freezeStatements counts the custodian's VACUUM statements for table.
func freezeStatements(proposals []autonomy.Proposal, table string) map[string]int {
	statements := map[string]int{}
	for _, p := range proposals {
		if len(p.TargetObjects) == 1 && p.TargetObjects[0] == table &&
			strings.Contains(p.SQL, "VACUUM") {
			statements[p.SQL]++
		}
	}
	return statements
}

func TestFreezeSingleProducerOneVacuumPerRedTable(t *testing.T) {
	pool := autonomyPool(t)
	ctx := context.Background()
	const table = "public.vacuum08_single"
	redReloptionTable(t, pool, table)
	cfg := config.DefaultConfig()
	custodian := newFreezeCustodian(pool, cfg, "testdb")
	horizons, err := custodian.Sampler().FreezeHorizons(ctx)
	if err != nil {
		t.Fatalf("sampler: %v", err)
	}
	findings := wraparoundRule(t)(&collector.Snapshot{}, nil, cfg,
		&analyzer.RuleExtras{FreezeHorizons: horizons})
	proposals, err := custodian.Scan(ctx)
	if err != nil {
		t.Fatalf("custodian scan: %v", err)
	}
	advised := adviseWraparound(t, custodian, table)
	diagnosed := 0
	for _, f := range findings {
		if f.ObjectIdentifier == table {
			diagnosed++
			if f.RecommendedSQL != "" {
				t.Fatalf("the analyzer produced a freeze statement: %q", f.RecommendedSQL)
			}
		}
	}
	statements := freezeStatements(proposals, table)
	if diagnosed != 1 || len(statements) != 1 || len(advised) != 1 {
		t.Fatalf("diagnosed %d, custodian statements %v, advised %d; want 1, one, 1",
			diagnosed, statements, len(advised))
	}
	if _, known := statements[advised[0].SQL]; !known {
		t.Fatalf("the advisor proposed its own statement %q (custodian: %v)",
			advised[0].SQL, statements)
	}
	for sql, n := range statements {
		if n != 1 || !strings.HasPrefix(sql, "VACUUM (FREEZE) ") || strings.HasSuffix(sql, ";") {
			t.Fatalf("custodian statement %q x%d, want one VACUUM (FREEZE) with no ';'", sql, n)
		}
	}
}

// One custodian per database runtime, whoever asks first and however
// many ask at once; the advisor and autonomy get that one.
func TestFreezeCustodianIsBuiltOncePerRuntime(t *testing.T) {
	rt := &databaseRuntime{cfg: config.DefaultConfig()}
	rt.spec.Name = "orders"
	var wg sync.WaitGroup
	got := make([]*autonomy.PostgresFreezeCustodian, 8)
	for i := range got {
		wg.Add(1)
		go func() { defer wg.Done(); got[i] = rt.freezeCustodian() }()
	}
	wg.Wait()
	for i, c := range got {
		if c == nil || c != got[0] {
			t.Fatalf("caller %d got %p, want the one custodian %p", i, c, got[0])
		}
	}
	advisor := newRunwayAdvisorFor(rt.freezeCustodian(), nil, rt.cfg, rt.spec.Name, nil, nil)
	if advisor.freeze != autonomy.Custodian(got[0]) {
		t.Fatal("the runway advisor built its own freeze custodian")
	}
	if rt.freezeCustodian().Sampler() == nil {
		t.Fatal("the runtime custodian has no sampler")
	}
}
