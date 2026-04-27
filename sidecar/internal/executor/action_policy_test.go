package executor

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestEvaluateActionPolicy_AutoSafeAllowsAnalyzeWithGuardrails(t *testing.T) {
	cfg := &config.Config{
		CloudEnvironment: "cloud-sql",
		Trust: config.TrustConfig{
			Level:     "autonomous",
			Tier3Safe: true,
		},
	}
	ctx := ActionPolicyContext{
		Config:          cfg,
		ExecutionMode:   "auto",
		RampStart:       time.Now().Add(-10 * 24 * time.Hour),
		SafeActionLimit: 3,
	}

	decision := EvaluateActionPolicy(AnalyzeTableContract(), ctx)

	if decision.Decision != PolicyDecisionExecute {
		t.Fatalf("Decision = %q, want %q",
			decision.Decision, PolicyDecisionExecute)
	}
	if decision.RequiresApproval {
		t.Fatalf("RequiresApproval = true, want false")
	}
	if len(decision.Guardrails) == 0 {
		t.Fatalf("expected guardrails")
	}
	if decision.RiskTier != "safe" {
		t.Fatalf("RiskTier = %q, want safe", decision.RiskTier)
	}
}

func TestEvaluateActionPolicy_BlocksSafeActionAtConcurrencyLimit(t *testing.T) {
	cfg := &config.Config{
		Trust: config.TrustConfig{
			Level:     "autonomous",
			Tier3Safe: true,
		},
	}
	ctx := ActionPolicyContext{
		Config:              cfg,
		ExecutionMode:       "auto",
		RampStart:           time.Now().Add(-10 * 24 * time.Hour),
		SafeActionLimit:     2,
		SafeActionsInFlight: 2,
	}

	decision := EvaluateActionPolicy(AnalyzeTableContract(), ctx)

	if decision.Decision != PolicyDecisionBlocked {
		t.Fatalf("Decision = %q, want blocked", decision.Decision)
	}
	if decision.BlockedReason != "safe action concurrency limit reached" {
		t.Fatalf("BlockedReason = %q", decision.BlockedReason)
	}
}

func TestEvaluateActionPolicy_ModerateActionQueuesOutsideWindow(t *testing.T) {
	cfg := &config.Config{
		Trust: config.TrustConfig{
			Level:             "autonomous",
			Tier3Moderate:     true,
			MaintenanceWindow: "0 2 * * *",
		},
	}
	contract := ActionContract{
		ActionType:    "create_index_concurrently",
		BaseRiskTier:  "moderate",
		RollbackClass: "reversible",
		PostChecks:    []string{"verify index is valid"},
	}
	ctx := ActionPolicyContext{
		Config:        cfg,
		ExecutionMode: "auto",
		Now:           time.Date(2026, 4, 27, 4, 30, 0, 0, time.UTC),
		RampStart:     time.Now().Add(-40 * 24 * time.Hour),
	}

	decision := EvaluateActionPolicy(contract, ctx)

	if decision.Decision != PolicyDecisionQueueApproval {
		t.Fatalf("Decision = %q, want queue_for_approval",
			decision.Decision)
	}
	if !decision.RequiresApproval || !decision.RequiresMaintenanceWindow {
		t.Fatalf("expected approval and maintenance window requirements: %#v",
			decision)
	}
	if decision.BlockedReason != "outside maintenance window" {
		t.Fatalf("BlockedReason = %q", decision.BlockedReason)
	}
}

func TestEvaluateActionPolicy_BlocksUnsupportedProvider(t *testing.T) {
	cfg := &config.Config{
		CloudEnvironment: "neon",
		Trust: config.TrustConfig{
			Level:     "autonomous",
			Tier3Safe: true,
		},
	}
	ctx := ActionPolicyContext{
		Config:        cfg,
		ExecutionMode: "auto",
		RampStart:     time.Now().Add(-10 * 24 * time.Hour),
	}

	decision := EvaluateActionPolicy(AnalyzeTableContract(), ctx)

	if decision.Decision != PolicyDecisionBlocked {
		t.Fatalf("Decision = %q, want blocked", decision.Decision)
	}
	if decision.BlockedReason != "provider neon is not supported" {
		t.Fatalf("BlockedReason = %q", decision.BlockedReason)
	}
}
