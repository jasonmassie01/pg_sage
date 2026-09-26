package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// Regression tests for G4-B01, G4-B02, G4-B16, G4-B18 and G4-B36.

var approvalGuardedActionTypes = []string{
	"drop_unused_index", "alter_table", "cancel_backend", "terminate_backend",
	"set_table_autovacuum", "reindex_concurrently", "apply_query_hint",
	"create_statistics", "prepare_query_rewrite", "promote_role_work_mem",
	"prepare_parameterized_query",
}

func TestPolicyContractMapsApprovalRequiredGuardrail(t *testing.T) {
	for _, actionType := range approvalGuardedActionTypes {
		contract, ok := ContractForActionType(actionType)
		if !ok {
			t.Fatalf("no contract for %s", actionType)
		}
		mapped := policyContract(contract)
		if len(mapped.Guardrails) != 1 ||
			mapped.Guardrails[0] != policy.GuardrailApprovalRequired {
			t.Fatalf("%s guardrails = %#v, want approval_required", actionType,
				mapped.Guardrails)
		}
		if err := policy.ValidateContract(*mapped); err != nil {
			t.Fatalf("%s mapped contract invalid: %v", actionType, err)
		}
	}
	create, _ := ContractForActionType("create_index_concurrently")
	if got := policyContract(create).Guardrails; len(got) != 0 {
		t.Fatalf("create_index_concurrently guardrails = %#v, want none", got)
	}
}

func TestIsApprovalRequiredGuardrailNormalizesSpelling(t *testing.T) {
	for _, value := range []string{
		"approval required", "approval_required", " Approval-Required ",
		"APPROVAL  REQUIRED",
	} {
		if !isApprovalRequiredGuardrail(value) {
			t.Fatalf("%q not recognised as approval guardrail", value)
		}
	}
	for _, value := range []string{
		"approval or autonomous moderate policy required", "manual review required", "",
	} {
		if isApprovalRequiredGuardrail(value) {
			t.Fatalf("%q wrongly recognised as approval guardrail", value)
		}
	}
}

func fullyEnabledGate(now time.Time) policy.Gate {
	doc := policy.UnattendedProfile()
	return policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return policy.RuntimeState{
				ExecutorEnabled: true, TrustLevel: policy.TrustAutonomous,
				ExecutionMode: policy.ExecutionAuto, Tier3Safe: true, Tier3Moderate: true,
				RampStart: now.Add(-90 * 24 * time.Hour), InConfiguredWindow: true,
			}, nil
		},
		ValidateSQL: ValidateExecutorSQL,
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return doc, nil
		},
		Now: func() time.Time { return now },
	})
}

func TestStandingGateQueuesApprovalGuardedFindings(t *testing.T) {
	exec := New(nil, &config.Config{}, nil, time.Time{}, func(string, string, ...any) {})
	exec.WithPolicyGate(fullyEnabledGate(time.Now()))
	for _, sql := range []string{
		"DROP INDEX CONCURRENTLY public.idx_x;",
		"REINDEX INDEX CONCURRENTLY public.idx_x",
		"ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.01)",
		"SELECT pg_cancel_backend(4711);",
	} {
		finding := analyzer.Finding{ObjectIdentifier: "public.t", RecommendedSQL: sql}
		decision := exec.evaluateFindingPolicy(context.Background(), finding, false)
		if decision.Decision == PolicyDecisionExecute {
			t.Fatalf("%q executed autonomously: %#v", sql, decision)
		}
	}
}

func TestLegacyPolicyQueuesApprovalGuardedContract(t *testing.T) {
	cfg := wave1PolicyConfig("autonomous")
	cfg.Trust.Tier3Moderate = true
	cfg.Trust.MaintenanceWindow = "always"
	enabled := true
	contract, _ := ContractForActionType("drop_unused_index")

	decision := EvaluateActionPolicy(contract, ActionPolicyContext{
		Config: cfg, ExecutionMode: "auto", ExecutorEnabled: &enabled,
		Now: time.Now(), RampStart: time.Now().Add(-90 * 24 * time.Hour),
	})

	if decision.Decision != PolicyDecisionQueueApproval {
		t.Fatalf("decision = %#v, want queue_for_approval", decision)
	}
}

func TestFeatureForFindingDerivesChangeClassFromContract(t *testing.T) {
	tests := map[string]string{
		"CREATE INDEX CONCURRENTLY idx ON public.t (a)":     "index",
		"DROP INDEX CONCURRENTLY public.idx":                "index",
		"REINDEX INDEX CONCURRENTLY public.idx":             "index",
		"ANALYZE public.t":                                  "analyze",
		"VACUUM public.t":                                   "vacuum",
		"ALTER TABLE public.t SET (autovacuum_enabled = on)": "autovacuum_tuning",
		"ALTER SYSTEM SET work_mem = '64MB'":                "config_guc",
		"SELECT pg_cancel_backend(42)":                      "backend_signal",
		"SELECT pg_terminate_backend(42)":                   "backend_signal",
		"INSERT INTO hint_plan.hints (norm_query_string) VALUES ('x')": "query_hint",
		"DELETE FROM hint_plan.hints WHERE id = 1":                      "query_hint",
		"ALTER TABLE public.t VALIDATE CONSTRAINT c":                    "schema_change",
		"TRUNCATE public.t":                                             "",
	}
	for sql, want := range tests {
		got := featureForFinding(analyzer.Finding{RecommendedSQL: sql})
		if got != want {
			t.Fatalf("featureForFinding(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestStandingRuntimeStateCarriesTrustCeilings(t *testing.T) {
	cfg := wave1PolicyConfig("autonomous")
	cfg.Trust.Tier3Safe = true
	cfg.Trust.Tier3Moderate = false
	cfg.Trust.MaintenanceWindow = "always"
	ramp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	exec := New(nil, cfg, nil, ramp, func(string, string, ...any) {})
	exec.emergencyStopFn = func(context.Context) bool { return false }

	state := exec.standingRuntimeState(context.Background(), policy.ActionRequest{})

	if !state.Tier3Safe || state.Tier3Moderate || !state.RampStart.Equal(ramp) ||
		!state.InConfiguredWindow || state.TrustLevel != "autonomous" {
		t.Fatalf("runtime state = %#v", state)
	}
	cfg.Trust.MaintenanceWindow = ""
	state = exec.standingRuntimeState(context.Background(), policy.ActionRequest{})
	if state.InConfiguredWindow {
		t.Fatal("empty trust.maintenance_window must not open the configured window")
	}
}

func TestPolicySnapshotWaitsForHotReloadWriter(t *testing.T) {
	cfg := wave1PolicyConfig("autonomous")
	exec := New(nil, cfg, nil, time.Time{}, func(string, string, ...any) {})
	config.LockForHotReload()
	done := make(chan struct{})
	go func() {
		exec.policySnapshot()
		close(done)
	}()
	select {
	case <-done:
		config.UnlockForHotReload()
		t.Fatal("policySnapshot read the config while a hot-reload writer held the lock")
	case <-time.After(100 * time.Millisecond):
	}
	config.UnlockForHotReload()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("policySnapshot did not complete after the writer released")
	}
}

func TestApprovalReadinessIgnoresAutoExecutionEligibility(t *testing.T) {
	tests := []struct {
		name  string
		mode  string
		trust string
		ramp  time.Duration
	}{
		{"approval mode with empty trust window", "approval", "advisory", 90},
		{"auto mode before moderate ramp", "auto", "autonomous", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := wave1PolicyConfig(tt.trust)
			cfg.Trust.MaintenanceWindow = ""
			exec := New(nil, cfg, nil, time.Now().Add(-tt.ramp*24*time.Hour),
				func(string, string, ...any) {})
			exec.SetExecutionMode(tt.mode)
			action := store.QueuedAction{
				ActionType: "create_index_concurrently", ActionRisk: "moderate",
				Status:      "pending",
				ProposedSQL: "CREATE INDEX CONCURRENTLY idx_o ON public.orders (id)",
				ExpiresAt:   time.Now().Add(time.Hour),
			}

			got := exec.ApprovalReadiness(action, time.Now())

			if !got.Eligible {
				t.Fatalf("operator approval refused: %q (%#v)", got.DeferReason, got.Policy)
			}
		})
	}
}

func TestApprovalReadinessStillRefusesObservationTrust(t *testing.T) {
	exec := New(nil, wave1PolicyConfig("observation"), nil, time.Time{},
		func(string, string, ...any) {})
	exec.SetExecutionMode("approval")
	action := store.QueuedAction{
		ActionType: "analyze_table", ActionRisk: "safe", Status: "pending",
		ProposedSQL: "ANALYZE public.orders", ExpiresAt: time.Now().Add(time.Hour),
	}

	got := exec.ApprovalReadiness(action, time.Now())

	if got.Eligible {
		t.Fatalf("observation trust readiness = %#v, want refused", got)
	}
}
