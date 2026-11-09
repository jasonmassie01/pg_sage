package api

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
)

// protectTable is a confirmed fact protecting one relation (the facts
// store is covered by its own tests; this one stands in for it).
type protectTable string

func (p protectTable) Bind(_ context.Context, req policy.ActionRequest) (
	[]policy.FactBinding, error) {
	for _, target := range req.TargetObjs {
		if target == string(p) {
			return []policy.FactBinding{{FactID: 11, Type: "app_managed", Subject: target,
				Summary: target + " is owned by the app's migrations", Object: target}}, nil
		}
	}
	return nil, nil
}

// providerCaseManager registers "prod" on pool with a real standing gate
// for the given provider ("" is self-managed PostgreSQL).
func providerCaseManager(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, provider string,
) (*fleet.DatabaseManager, *executor.Executor) {
	t.Helper()
	cfg := caseAutonomousConfig()
	cfg.CloudEnvironment = provider
	exec := executor.New(pool, cfg, time.Now().Add(-40*24*time.Hour),
		func(string, string, ...any) {})
	exec.SetExecutionMode("auto")
	if err := exec.EnableStandingPolicy(ctx, "unattended", nil); err != nil {
		t.Fatalf("enable standing policy: %v", err)
	}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name: "prod", Config: config.DatabaseConfig{Name: "prod", ExecutionMode: "auto"},
		Pool: pool, Executor: exec,
		Status: &fleet.InstanceStatus{Platform: provider,
			Capabilities: fleet.ProviderCapabilities{Provider: provider}},
	})
	return mgr, exec
}

func seedFinding(t *testing.T, ctx context.Context, category, objectType, object, sql string) {
	t.Helper()
	pool, _ := phase2RequireDB(t)
	_, err := pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql, status)
		VALUES ($1, 'warning', $2, $3, $1 || ' on ' || $3, '{}'::jsonb, 'act', $4, 'open')`,
		category, objectType, object, sql)
	if err != nil {
		t.Fatalf("seed %s finding: %v", category, err)
	}
}

func findingForTest(category, objectType, object, sql string) analyzer.Finding {
	return analyzer.Finding{Category: category, ObjectType: objectType,
		ObjectIdentifier: object, RecommendedSQL: sql, Detail: map[string]any{}}
}

// candidateFor is the only candidate of the case whose SQL is sql.
func candidateFor(t *testing.T, all []cases.Case, sql string) cases.ActionCandidate {
	t.Helper()
	for _, c := range all {
		for _, candidate := range c.ActionCandidates {
			if candidate.ProposedSQL == sql {
				return candidate
			}
		}
	}
	t.Fatalf("no candidate proposes %q in %d cases", sql, len(all))
	return cases.ActionCandidate{}
}

// An advisor ALTER SYSTEM finding carries the executor's alter_system_guc
// contract, and its verdict is the gate's for that statement.
func TestCasesAlterSystemProjectsExecutorContract(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	mgr, exec := providerCaseManager(t, ctx, pool, "")
	sql := "ALTER SYSTEM SET work_mem = '64MB'"
	seedFinding(t, ctx, "config", "setting", "work_mem", sql)

	all, err := queryProjectedCases(ctx, mgr, "prod")
	if err != nil {
		t.Fatalf("queryProjectedCases: %v", err)
	}
	got := candidateFor(t, all, sql)
	if got.ActionType != "alter_system_guc" || got.RiskTier != "moderate" {
		t.Fatalf("candidate = %s/%s, want alter_system_guc/moderate", got.ActionType,
			got.RiskTier)
	}
	contract, _ := executor.ContractForActionType("alter_system_guc")
	want := exec.ExplainActions(ctx, []executor.ExplainActionRequest{{Contract: contract,
		Finding: findingForTest("config", "setting", "work_mem", sql)}}, false)[0]
	if got.PolicyDecision == nil || got.PolicyDecision.Decision != want.Decision ||
		got.PolicyDecision.BlockedReason != want.BlockedReason {
		t.Fatalf("verdict = %+v, want the gate's %s/%q", got.PolicyDecision, want.Decision,
			want.BlockedReason)
	}
	if got.BlockedReason == "unknown action type" {
		t.Fatal("the GUC contract was not explained")
	}
}

// On Lakebase the advisor emits ALTER DATABASE, which projects the
// alter_database_guc contract Lakebase supports; it is not withheld as a
// provider-managed setting.
func TestCasesLakebaseAlterDatabaseProjectsDatabaseGUC(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	mgr, _ := providerCaseManager(t, ctx, pool, "lakebase")
	sql := `ALTER DATABASE "prod" SET work_mem = '64MB'`
	seedFinding(t, ctx, "config", "setting", "work_mem", sql)

	all, err := queryProjectedCases(ctx, mgr, "prod")
	if err != nil {
		t.Fatalf("queryProjectedCases: %v", err)
	}
	got := candidateFor(t, all, sql)
	if got.ActionType != "alter_database_guc" || got.RiskTier != "moderate" {
		t.Fatalf("candidate = %s/%s, want alter_database_guc/moderate", got.ActionType,
			got.RiskTier)
	}
	d := got.PolicyDecision
	if d == nil || d.Provider != "lakebase" || d.BlockedReason == "provider_managed_setting" ||
		d.BlockedReason == string(policy.ReasonProviderUnsupported) {
		t.Fatalf("lakebase verdict = %+v, want the gate's verdict for a supported contract", d)
	}
}

// CHECK-07: a confirmed fact protecting public.orders blocks that table's
// VACUUM case with the gate's reason, and leaves public.items unblocked.
func TestCasesProtectedTableIsBlockedByTheGate(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	mgr, exec := providerCaseManager(t, ctx, pool, "")
	exec.WithFactBinder(protectTable("public.orders"))
	seedFinding(t, ctx, "table_bloat", "table", "public.orders", "VACUUM public.orders")
	seedFinding(t, ctx, "table_bloat", "table", "public.items", "VACUUM public.items")

	all, err := queryProjectedCases(ctx, mgr, "prod")
	if err != nil {
		t.Fatalf("queryProjectedCases: %v", err)
	}
	orders := candidateFor(t, all, "VACUUM public.orders")
	if d := orders.PolicyDecision; d == nil || d.Decision != executor.PolicyDecisionBlocked ||
		d.BlockedReason != string(policy.ReasonBoundByFact) ||
		orders.BlockedReason != string(policy.ReasonBoundByFact) {
		t.Fatalf("protected orders = %+v / %+v, want blocked bound_by_fact", orders,
			orders.PolicyDecision)
	}
	items := candidateFor(t, all, "VACUUM public.items")
	if d := items.PolicyDecision; d == nil || d.Decision == executor.PolicyDecisionBlocked ||
		items.BlockedReason != "" {
		t.Fatalf("unprotected items = %+v / %+v, want the gate's unblocked verdict", items,
			items.PolicyDecision)
	}
}
