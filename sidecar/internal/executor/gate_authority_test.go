package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// G4-I01: provider support was enforced only by the legacy engine, so the
// standing gate authorized actions a provider cannot run.
func TestStandingGateReceivesProviderSupport(t *testing.T) {
	contract, ok := ContractForActionType("analyze_table")
	if !ok {
		t.Fatal("analyze_table contract missing")
	}
	got := policyContract(contract).ProviderSupport
	if len(got) == 0 || len(got) != len(contract.ProviderSupport) {
		t.Fatalf("gate contract provider support = %v, want %v",
			got, contract.ProviderSupport)
	}
	exec := New(nil, &config.Config{CloudEnvironment: "azure"}, nil, time.Time{}, noopExecLog)
	exec.emergencyStopFn = func(context.Context) bool { return false }
	state := exec.standingRuntimeState(context.Background(), policy.ActionRequest{})
	if state.Provider != "azure" {
		t.Fatalf("runtime provider = %q, want azure", state.Provider)
	}
}

// Without a standing gate nothing may execute: the legacy engine is not a
// fallback authority.
func TestFindingPolicyFailsClosedWithoutGate(t *testing.T) {
	exec := New(nil, autonomousTestConfig(), nil, time.Now().Add(-90*24*time.Hour), noopExecLog)
	finding := analyzer.Finding{ObjectIdentifier: "public.t",
		RecommendedSQL: "ANALYZE public.t"}
	got := exec.evaluateFindingPolicy(context.Background(), finding, false)
	if got.Decision != PolicyDecisionBlocked || got.BlockedReason != reasonNoStandingPolicy {
		t.Fatalf("decision without gate = %#v, want blocked/%s", got, reasonNoStandingPolicy)
	}
	if exec.authorizeCreatedIndexRevert(context.Background(),
		"DROP INDEX CONCURRENTLY public.idx_x", "public.idx_x") {
		t.Fatal("created-index revert authorized without a standing gate")
	}
}

// Proposal metadata shows the gate's verdict, evaluated with Explain so a
// proposal writes no ledger decision.
func TestProposalMetadataUsesGateExplain(t *testing.T) {
	now := time.Now()
	recorded := 0
	exec := New(nil, autonomousTestConfig(), nil, now.Add(-90*24*time.Hour), noopExecLog)
	exec.WithPolicyGate(explainTestGate(now, &recorded))
	finding := analyzer.Finding{Category: "stale_stats", ObjectType: "table",
		ObjectIdentifier: "public.orders", RecommendedSQL: "ANALYZE public.orders"}

	got := exec.buildApprovalProposalMetadata(finding, now)

	if got.PolicyDecision != PolicyDecisionExecute {
		t.Fatalf("proposal policy decision = %q, want the gate's execute", got.PolicyDecision)
	}
	if len(got.Guardrails) == 0 {
		t.Fatal("proposal lost the contract's descriptive guardrails")
	}
	if recorded != 0 {
		t.Fatalf("proposal recorded %d ledger decisions, want 0", recorded)
	}
	exec.WithPolicyGate(nil)
	if got := exec.buildApprovalProposalMetadata(finding, now); got.PolicyDecision !=
		PolicyDecisionBlocked {
		t.Fatalf("proposal without gate = %q, want blocked", got.PolicyDecision)
	}
}

func explainTestGate(now time.Time, recorded *int) policy.Gate {
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
		RecordDecision: func(context.Context, policy.ActionRequest, policy.Decision) (string, error) {
			*recorded++
			return "recorded", nil
		},
		Now: func() time.Time { return now },
	})
}

func autonomousTestConfig() *config.Config {
	return &config.Config{Trust: config.TrustConfig{
		Level: "autonomous", Tier3Safe: true, Tier3Moderate: true,
	}}
}

func noopExecLog(string, string, ...any) {}
