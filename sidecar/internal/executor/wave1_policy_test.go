package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

func TestWave1ActionPolicyMatrix(t *testing.T) {
	now := time.Date(2026, 7, 18, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name, trust, mode, risk, want string
	}{
		{"manual advisory safe", "advisory", "manual", "safe", PolicyDecisionObserveOnly},
		{"manual autonomous moderate", "autonomous", "manual", "moderate", PolicyDecisionObserveOnly},
		{"observation manual safe", "observation", "manual", "safe", PolicyDecisionObserveOnly},
		{
			"observation approval moderate", "observation", "approval",
			"moderate", PolicyDecisionObserveOnly,
		},
		{"observation auto safe", "observation", "auto", "safe", PolicyDecisionObserveOnly},
		{"advisory auto safe", "advisory", "auto", "safe", PolicyDecisionExecute},
		{"advisory auto moderate", "advisory", "auto", "moderate", PolicyDecisionQueueApproval},
		{"advisory auto high", "advisory", "auto", "high", PolicyDecisionQueueApproval},
		{"advisory approval safe", "advisory", "approval", "safe", PolicyDecisionQueueApproval},
		{"advisory approval moderate", "advisory", "approval", "moderate", PolicyDecisionQueueApproval},
		{"autonomous auto safe", "autonomous", "auto", "safe", PolicyDecisionExecute},
		{"autonomous auto moderate", "autonomous", "auto", "moderate", PolicyDecisionExecute},
		{"autonomous auto high", "autonomous", "auto", "high", PolicyDecisionQueueApproval},
		{"autonomous approval high", "autonomous", "approval", "high", PolicyDecisionQueueApproval},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := wave1PolicyConfig(tc.trust)
			got := EvaluateActionPolicy(wave1Contract(tc.risk), ActionPolicyContext{
				Config:        cfg,
				ExecutionMode: tc.mode,
				Now:           now,
				RampStart:     now.Add(-40 * 24 * time.Hour),
			})
			if got.Decision != tc.want {
				t.Fatalf("Decision = %q, want %q: %#v", got.Decision, tc.want, got)
			}
		})
	}
}

func TestCHECK18DesignatedFleetPolicyCanary(t *testing.T) {
	now := time.Date(2026, 7, 19, 3, 0, 0, 0, time.UTC)
	executorEnabled := true
	stages := []struct {
		name  string
		trust string
		mode  string
		risk  string
		want  string
	}{
		{"observation manual", "observation", "manual", "safe", PolicyDecisionObserveOnly},
		{"advisory approval", "advisory", "approval", "safe", PolicyDecisionQueueApproval},
		{"advisory auto", "advisory", "auto", "safe", PolicyDecisionExecute},
		{"autonomous auto", "autonomous", "auto", "moderate", PolicyDecisionExecute},
		{"autonomous high risk", "autonomous", "auto", "high", PolicyDecisionQueueApproval},
	}

	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			got := EvaluateActionPolicy(wave1Contract(stage.risk), ActionPolicyContext{
				Config:          wave1PolicyConfig(stage.trust),
				ExecutionMode:   stage.mode,
				ExecutorEnabled: &executorEnabled,
				Now:             now,
				RampStart:       now.Add(-40 * 24 * time.Hour),
			})
			if got.Decision != stage.want {
				t.Fatalf("decision = %q, want %q: %#v", got.Decision, stage.want, got)
			}
		})
	}
}

func TestWave1ActionPolicyHardBlocks(t *testing.T) {
	disabled := false
	base := ActionPolicyContext{
		Config:        wave1PolicyConfig("autonomous"),
		ExecutionMode: "auto",
		RampStart:     time.Now().Add(-40 * 24 * time.Hour),
	}
	tests := []struct {
		name string
		edit func(*ActionPolicyContext)
		want string
	}{
		{
			"executor disabled",
			func(c *ActionPolicyContext) { c.ExecutorEnabled = &disabled },
			"executor is disabled",
		},
		{
			"emergency stop",
			func(c *ActionPolicyContext) { c.EmergencyStop = true },
			"emergency stop is active",
		},
		{"replica", func(c *ActionPolicyContext) { c.IsReplica = true }, "target database is a replica"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := base
			tc.edit(&ctx)
			got := EvaluateActionPolicy(wave1Contract("safe"), ctx)
			if got.Decision != PolicyDecisionBlocked || got.BlockedReason != tc.want {
				t.Fatalf("decision = %#v, want blocked reason %q", got, tc.want)
			}
		})
	}
}

func TestWave1ActionPolicyRejectsUnknownTrustLevels(t *testing.T) {
	for _, trust := range []string{"", "invalid"} {
		t.Run(trust, func(t *testing.T) {
			got := EvaluateActionPolicy(wave1Contract("safe"), ActionPolicyContext{
				Config:        wave1PolicyConfig(trust),
				ExecutionMode: "auto",
				RampStart:     time.Now().Add(-40 * 24 * time.Hour),
			})
			if got.Decision != PolicyDecisionBlocked ||
				got.BlockedReason != "unknown trust level" {
				t.Fatalf("decision = %#v, want fail-closed trust rejection", got)
			}
		})
	}
}

func TestWave1ApprovalDoesNotUseAutoEligibilityGate(t *testing.T) {
	ctx := ActionPolicyContext{
		Config:        wave1PolicyConfig("advisory"),
		ExecutionMode: "approval",
		RampStart:     time.Now(),
	}
	for _, risk := range []string{"safe", "moderate", "high"} {
		got := EvaluateActionPolicy(wave1Contract(risk), ctx)
		if got.Decision != PolicyDecisionQueueApproval {
			t.Errorf("risk %q decision = %q, want queue_for_approval", risk, got.Decision)
		}
	}
}

func TestWave1TypedContractRiskGovernsFinding(t *testing.T) {
	e := newWave1Executor("advisory", "auto")
	finding := analyzer.Finding{
		RecommendedSQL: "CREATE INDEX CONCURRENTLY idx_orders ON orders (id)",
		ActionRisk:     "safe",
	}
	got := e.evaluateFindingPolicy(context.Background(), finding, false)
	if got.RiskTier != "moderate" {
		t.Fatalf("RiskTier = %q, want typed contract risk moderate", got.RiskTier)
	}
	if got.Decision != PolicyDecisionQueueApproval {
		t.Fatalf("Decision = %q, want queue_for_approval", got.Decision)
	}
}

func TestWave1ContractMappingRejectsMismatchedOrUnsupportedSQL(t *testing.T) {
	tests := []string{
		"CREATE INDEX idx_orders ON orders (id)",
		"DROP INDEX idx_orders",
		"REINDEX INDEX idx_orders",
		`REINDEX INDEX "idx CONCURRENTLY orders"`,
		"VACUUM FULL public.orders",
		"VACUUM (FULL, ANALYZE) public.orders",
		"CREATE STATISTICS st_orders ON id, status FROM orders",
		"ALTER ROLE app SET work_mem = '64MB'",
		"ALTER SYSTEM SET session_preload_libraries = 'unsafe'",
		"ALTER DATABASE app SET session_preload_libraries = 'unsafe'",
		"INSERT INTO public.hints VALUES (1)",
	}
	for _, sql := range tests {
		t.Run(sql, func(t *testing.T) {
			finding := analyzer.Finding{RecommendedSQL: sql, ActionRisk: "safe"}
			if contract, ok := contractForFinding(finding); ok {
				t.Fatalf("unexpected contract %#v for unsupported SQL", contract)
			}
		})
	}
}

func TestWave1QuotedIdentifiersCannotSpoofActionClassification(t *testing.T) {
	tests := map[string]string{
		`REINDEX INDEX "idx CONCURRENTLY orders"`:                     "",
		`ALTER TABLE "tenant AUTOVACUUM_enabled" SET TABLESPACE fast`: "alter_table",
	}
	for sql, want := range tests {
		t.Run(sql, func(t *testing.T) {
			contract, ok := contractForFinding(analyzer.Finding{RecommendedSQL: sql})
			if want == "" {
				if ok {
					t.Fatalf("contract = %#v, want unsupported", contract)
				}
				return
			}
			if !ok || contract.ActionType != want {
				t.Fatalf("contract = %#v, ok=%v, want %q", contract, ok, want)
			}
		})
	}
}

func TestWave1ContractMappingAcceptsExactGuardrailedSQL(t *testing.T) {
	const createIndexAction = "create_index_concurrently"
	tests := map[string]string{
		"ANALYZE public.orders":                                                   "analyze_table",
		"CREATE UNIQUE INDEX CONCURRENTLY idx ON orders (id)":                     createIndexAction,
		"DROP INDEX CONCURRENTLY idx_orders":                                      "drop_unused_index",
		"REINDEX INDEX CONCURRENTLY idx_orders":                                   "reindex_concurrently",
		"VACUUM (ANALYZE) public.orders":                                          "vacuum_table",
		"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.1)":    "set_table_autovacuum",
		"SELECT pg_cancel_backend(42)":                                            "cancel_backend",
		"SELECT pg_terminate_backend(42)":                                         "terminate_backend",
		"ALTER SYSTEM SET work_mem = '64MB'":                                      "alter_system_guc",
		"ALTER DATABASE app SET work_mem = '64MB'":                                "alter_database_guc",
		"INSERT INTO hint_plan.hints (query_id, hints) VALUES (42, 'SeqScan(t)')": "apply_query_hint",
		"DELETE FROM hint_plan.hints WHERE query_id = 42":                         "retire_query_hint",
	}
	for sql, want := range tests {
		t.Run(sql, func(t *testing.T) {
			contract, ok := contractForFinding(analyzer.Finding{RecommendedSQL: sql})
			if !ok || contract.ActionType != want {
				t.Fatalf("contract = %#v, ok=%v, want %q", contract, ok, want)
			}
		})
	}
}

func TestWave1AlterDatabaseUsesGUCAllowlist(t *testing.T) {
	if err := ValidateExecutorSQL(
		"ALTER DATABASE app SET work_mem = '64MB'",
	); err != nil {
		t.Fatalf("allowed GUC rejected: %v", err)
	}
	if err := ValidateExecutorSQL(
		"ALTER DATABASE app SET session_preload_libraries = 'unsafe'",
	); err == nil {
		t.Fatal("non-allowlisted ALTER DATABASE GUC was accepted")
	}
	if err := ValidateExecutorSQL(
		`ALTER DATABASE "tenant SET work_mem" SET work_mem = '64MB'`,
	); err != nil {
		t.Fatalf("quoted identifier containing SET confused parser: %v", err)
	}
	if err := ValidateExecutorSQL(
		`ALTER DATABASE "tenant SET work_mem = 1" SET session_preload_libraries = 'unsafe'`,
	); err == nil {
		t.Fatal("quoted identifier hid a non-allowlisted real GUC")
	}
	if err := ValidateExecutorSQL(
		`ALTER DATABASE "tenant RESET work_mem" RESET work_mem`,
	); err != nil {
		t.Fatalf("quoted identifier containing RESET confused parser: %v", err)
	}
	if err := ValidateExecutorSQL(
		`ALTER DATABASE "tenant RESET work_mem" RESET session_preload_libraries`,
	); err == nil {
		t.Fatal("quoted identifier hid a non-allowlisted real RESET GUC")
	}
}

func TestWave1PolicySettersAreConcurrentSafe(t *testing.T) {
	e := newWave1Executor("observation", "auto")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			levels := []string{"observation", "advisory", "autonomous", ""}
			_ = e.SetTrustLevel(levels[i%len(levels)])
			_ = e.TrustLevel()
		}(i)
		go func(i int) {
			defer wg.Done()
			modes := []string{"manual", "approval", "auto"}
			e.SetExecutionMode(modes[i%len(modes)])
			_ = e.ExecutionMode()
		}(i)
	}
	wg.Wait()
}

func TestWave1EveryActionUsesLatestSafetyPolicy(t *testing.T) {
	e := newWave1Executor("autonomous", "auto")
	finding := analyzer.Finding{
		RecommendedSQL: "ANALYZE public.orders",
		ActionRisk:     "safe",
	}
	checks := 0
	e.emergencyStopFn = func(context.Context) bool {
		checks++
		return checks > 1
	}
	first := e.evaluateFindingPolicy(context.Background(), finding, false)
	second := e.evaluateFindingPolicy(context.Background(), finding, false)
	if first.Decision != PolicyDecisionExecute {
		t.Fatalf("first decision = %q, want execute", first.Decision)
	}
	if second.Decision != PolicyDecisionBlocked ||
		second.BlockedReason != "emergency stop is active" {
		t.Fatalf("second decision = %#v, want latest emergency stop block", second)
	}

	e.emergencyStopFn = func(context.Context) bool { return false }
	e.SetExecutionMode("manual")
	third := e.evaluateFindingPolicy(context.Background(), finding, false)
	if third.Decision != PolicyDecisionObserveOnly {
		t.Fatalf("decision after mode change = %q, want observe_only", third.Decision)
	}
}

func TestWave1ManualMutationsHonorHardGates(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Executor)
		want  string
	}{
		{"executor disabled", func(e *Executor) { e.SetExecutorEnabled(false) }, "executor is disabled"},
		{
			"observation",
			func(e *Executor) { _ = e.SetTrustLevel("observation") },
			"observation trust is cases only",
		},
		{"empty trust", func(e *Executor) { e.cfg.Trust.Level = "" }, "unknown trust level"},
		{"invalid trust", func(e *Executor) { e.cfg.Trust.Level = "invalid" }, "unknown trust level"},
		{"emergency stop", func(e *Executor) {
			e.emergencyStopFn = func(context.Context) bool { return true }
		}, "emergency stop active"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newWave1Executor("advisory", "manual")
			tc.setup(e)
			_, err := e.ExecuteManual(context.Background(), 1,
				"ANALYZE public.orders", "", nil)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestWave1RollbackHonorsExecutorDisabled(t *testing.T) {
	e := newWave1Executor("advisory", "manual")
	e.SetExecutorEnabled(false)
	err := e.RollbackAction(context.Background(), 1, "test")
	if err == nil || err.Error() != "executor is disabled" {
		t.Fatalf("error = %v, want executor is disabled", err)
	}
}

func wave1PolicyConfig(level string) *config.Config {
	return &config.Config{Trust: config.TrustConfig{
		Level:             level,
		Tier3Safe:         true,
		Tier3Moderate:     true,
		MaintenanceWindow: "always",
	}}
}

func wave1Contract(risk string) ActionContract {
	return ActionContract{
		ActionType:      "test_" + risk,
		BaseRiskTier:    risk,
		ProviderSupport: []string{"postgres"},
		PostChecks:      []string{"verify"},
		RollbackClass:   "reversible",
	}
}

func newWave1Executor(trust, mode string) *Executor {
	e := New(nil, wave1PolicyConfig(trust), nil,
		time.Now().Add(-40*24*time.Hour), func(string, string, ...any) {})
	e.emergencyStopFn = func(context.Context) bool { return false }
	e.SetExecutionMode(mode)
	return e
}
