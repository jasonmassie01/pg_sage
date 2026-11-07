package executor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/policy"
)

// vacuum-02: a holder escalation (prepared transaction, slot, standby or
// protected session) is a plan typed by its response kind as a high-risk
// diagnose_freeze_blockers decision of feature freeze_blocker, never a
// bloat plan; a session holder keeps the backend-signal contract of its
// SQL. No concurrency test: custodianRequest is a pure mapping.

func holderEscalation() CustodianProposal {
	return CustodianProposal{Feature: "freeze_blocker",
		ResponseKind:  string(freeze.ResponseEscalateHolder),
		TargetObjects: []string{"public.orders", "public.items"},
		Evidence: map[string]any{"plan": "Escalate prepared transaction",
			"holder_kind": "prepared_xact", "holder_name": "5d41402a"}}
}

func TestCustodianRequestTypesHolderEscalations(t *testing.T) {
	request := custodianRequest(holderEscalation())
	if request.Contract == nil || request.Contract.ActionType != "diagnose_freeze_blockers" ||
		request.Contract.RiskTier != policy.RiskHigh {
		t.Fatalf("escalation contract = %#v", request.Contract)
	}
	if request.Feature != "freeze_blocker" || request.SQL != "" ||
		request.Evidence["action_type"] != "diagnose_freeze_blockers" ||
		len(request.TargetObjs) != 2 {
		t.Fatalf("escalation request = %#v", request)
	}
	// The incident diagnostic keeps its own, read-only safe tier.
	if contract, ok := ContractForActionType("diagnose_freeze_blockers"); !ok ||
		contract.BaseRiskTier != "safe" {
		t.Fatalf("diagnose_freeze_blockers contract = %#v, %v", contract, ok)
	}
}

func TestCustodianRequestKeepsTheSignalContractForSessions(t *testing.T) {
	for kind, actionType := range map[freeze.ResponseKind]string{
		freeze.ResponseCancelBlocker:    "cancel_backend",
		freeze.ResponseTerminateBlocker: "terminate_backend",
	} {
		function := map[string]string{"cancel_backend": "pg_cancel_backend",
			"terminate_backend": "pg_terminate_backend"}[actionType]
		request := custodianRequest(CustodianProposal{Feature: "freeze_blocker",
			ResponseKind: string(kind), SQL: "SELECT " + function + "(42)",
			TargetObjects: []string{"public.orders"}})
		if request.Contract == nil || request.Contract.ActionType != actionType {
			t.Fatalf("%s contract = %#v, want %s", kind, request.Contract, actionType)
		}
	}
}

// Through the real standing gate the escalation parks at high: a plan
// has no SQL to validate, so nothing can execute it.
func TestHolderEscalationParksHighThroughTheStandingGate(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Trust.Level, cfg.Trust.Tier3Safe = "autonomous", true
	exec := New(nil, cfg, zeroTime(), func(string, string, ...any) {})
	exec.SetExecutionMode("auto")
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	exec.EnableStandingPolicyDocument(policy.UnattendedProfile(), nil)
	decision := exec.EvaluateCustodianProposal(context.Background(), holderEscalation())
	if decision.Decision != PolicyDecisionParked || decision.RiskTier != "high" {
		t.Fatalf("holder escalation decision = %#v, want parked at high", decision)
	}
}
