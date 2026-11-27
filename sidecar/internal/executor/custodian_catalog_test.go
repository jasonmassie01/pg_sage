package executor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/policy"
)

// vacuum-08: the freeze custodian's plan for red system catalogs carries
// no SQL; it must reach the gate typed by its response kind, as a
// high-risk review-only plan, never as an untyped (no_typed_contract)
// request. No concurrency test: custodianRequest is a pure mapping.

func catalogFreezePlan() CustodianProposal {
	return CustodianProposal{Feature: "freeze",
		ResponseKind:  string(freeze.ResponsePlanCatalogFreeze),
		TargetObjects: []string{"pg_catalog.pg_class", "pg_catalog.pg_attribute"},
		Evidence: map[string]any{"plan": "System catalogs are near their freeze maximum",
			"plan_only": true}}
}

func TestCustodianRequestTypesCatalogFreezePlans(t *testing.T) {
	request := custodianRequest(catalogFreezePlan())
	if request.Contract == nil || request.Contract.ActionType != "diagnose_freeze_blockers" ||
		request.Contract.RiskTier != policy.RiskHigh {
		t.Fatalf("catalog plan contract = %#v, want high diagnose_freeze_blockers",
			request.Contract)
	}
	if request.SQL != "" || request.Evidence["action_type"] != "diagnose_freeze_blockers" ||
		request.IncidentFamily == "" || len(request.TargetObjs) != 2 {
		t.Fatalf("catalog plan request = %#v", request)
	}
}

// Through the real standing gate the catalog plan parks at high, as the
// holder escalations do: nothing can execute a plan, and a catalog VACUUM
// is an operator's call.
func TestCatalogFreezePlanParksHighThroughTheStandingGate(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Trust.Level, cfg.Trust.Tier3Safe = "autonomous", true
	exec := New(nil, cfg, zeroTime(), func(string, string, ...any) {})
	exec.SetExecutionMode("auto")
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	exec.EnableStandingPolicyDocument(policy.UnattendedProfile(), nil)
	decision := exec.EvaluateCustodianProposal(context.Background(), catalogFreezePlan())
	if decision.Decision != PolicyDecisionParked || decision.RiskTier != "high" {
		t.Fatalf("catalog plan decision = %#v, want parked at high", decision)
	}
}
