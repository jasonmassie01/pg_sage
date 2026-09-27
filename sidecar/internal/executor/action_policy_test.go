package executor

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// These scenarios predate the standing gate; they now assert the gate's
// verdict through policyVerdict, the executor's only policy authority.

func TestActionPolicy_AutoSafeAllowsAnalyzeWithGuardrails(t *testing.T) {
	cfg := &config.Config{
		CloudEnvironment: "cloud-sql",
		Trust:            config.TrustConfig{Level: "autonomous", Tier3Safe: true},
	}
	decision := policyVerdict(AnalyzeTableContract(), verdictInput{
		cfg: cfg, rampStart: time.Now().Add(-10 * 24 * time.Hour),
	})

	if decision.Decision != PolicyDecisionExecute || decision.RequiresApproval {
		t.Fatalf("decision = %#v, want execute without approval", decision)
	}
	if len(decision.Guardrails) == 0 || decision.RiskTier != "safe" {
		t.Fatalf("decision = %#v, want safe tier with guardrails", decision)
	}
}

func TestActionPolicyApprovalGuardrailAlwaysQueues(t *testing.T) {
	contract := AnalyzeTableContract()
	contract.Guardrails = append(contract.Guardrails, "approval_required")
	cfg := &config.Config{Trust: config.TrustConfig{Level: "autonomous", Tier3Safe: true}}
	decision := policyVerdict(contract, verdictInput{
		cfg: cfg, rampStart: time.Now().Add(-40 * 24 * time.Hour),
	})
	if decision.Decision != PolicyDecisionQueueApproval || !decision.RequiresApproval {
		t.Fatalf("approval guardrail decision = %#v", decision)
	}
}

func TestActionPolicy_ModerateActionBlocksOutsideWindow(t *testing.T) {
	cfg := &config.Config{Trust: config.TrustConfig{
		Level: "autonomous", Tier3Moderate: true, MaintenanceWindow: "0 2 * * *",
	}}
	now := time.Date(2026, 4, 27, 4, 30, 0, 0, time.UTC)
	decision := policyVerdict(riskContract("moderate"), verdictInput{
		cfg: cfg, now: now, rampStart: now.Add(-40 * 24 * time.Hour),
	})

	if decision.Decision != PolicyDecisionBlocked {
		t.Fatalf("Decision = %q, want blocked", decision.Decision)
	}
	if decision.RequiresApproval || !decision.RequiresMaintenanceWindow {
		t.Fatalf("expected maintenance-window requirement without approval: %#v", decision)
	}
	if decision.BlockedReason != "outside_maintenance_window" {
		t.Fatalf("BlockedReason = %q", decision.BlockedReason)
	}
}

func TestActionPolicy_BlocksUnsupportedProvider(t *testing.T) {
	cfg := &config.Config{
		CloudEnvironment: "unknown-provider",
		Trust:            config.TrustConfig{Level: "autonomous", Tier3Safe: true},
	}
	decision := policyVerdict(AnalyzeTableContract(), verdictInput{
		cfg: cfg, rampStart: time.Now().Add(-10 * 24 * time.Hour),
	})

	if decision.Decision != PolicyDecisionBlocked ||
		decision.BlockedReason != "provider_unsupported" ||
		decision.Detail != "provider unknown-provider" {
		t.Fatalf("decision = %#v, want provider_unsupported", decision)
	}
}

func TestActionPolicy_ReadOnlyAutoAllowsReplicaDiagnostics(t *testing.T) {
	cfg := &config.Config{
		CloudEnvironment: "postgres",
		Trust:            config.TrustConfig{Level: "autonomous"},
	}
	contract, ok := ContractForActionType("diagnose_standby_conflicts")
	if !ok {
		t.Fatal("diagnose_standby_conflicts contract missing")
	}
	decision := policyVerdict(contract, verdictInput{cfg: cfg, isReplica: true})

	if decision.Decision != PolicyDecisionExecute || decision.BlockedReason != "" {
		t.Fatalf("decision = %#v, want execute", decision)
	}
}
