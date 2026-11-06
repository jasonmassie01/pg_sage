package executor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/policy"
)

// vacuum-04: the custodian's plan-only bloat responses are typed by their
// response kind, never by Evidence["plan"], so they reach the ledger as
// high-risk bloat_remediation decisions instead of failing its CHECK.
// No concurrency test: custodianRequest is a pure mapping.

func bloatProposal(kind freeze.ResponseKind) CustodianProposal {
	return CustodianProposal{Feature: "bloat_remediation", ResponseKind: string(kind),
		TargetObjects: []string{"public.orders"},
		Evidence:      map[string]any{"plan": "Plan pg_repack --table=public.orders"}}
}

func TestCustodianRequestTypesBloatPlansByKind(t *testing.T) {
	for _, kind := range []freeze.ResponseKind{
		freeze.ResponsePlanRepack, freeze.ResponseParkBloat,
	} {
		request := custodianRequest(bloatProposal(kind))
		if request.Contract == nil ||
			request.Contract.ActionType != "plan_bloat_remediation" ||
			request.Contract.RiskTier != policy.RiskHigh ||
			request.Contract.RollbackClass != policy.RollbackClass("forward_fix_only") {
			t.Fatalf("%s contract = %#v", kind, request.Contract)
		}
		if request.Feature != "bloat_remediation" || request.SQL != "" ||
			request.IncidentFamily != "" {
			t.Fatalf("%s request = %#v", kind, request)
		}
	}
}

func TestCustodianRequestIgnoresPlanEvidenceWithoutABloatKind(t *testing.T) {
	cases := map[string]CustodianProposal{
		// A future Plan-only holder escalation carries Evidence["plan"] too.
		"plan evidence, no kind": {Feature: "freeze", TargetObjects: []string{"public.orders"},
			Evidence: map[string]any{"plan": "Escalate the xmin holder"}},
		"unknown kind": {Feature: "freeze", ResponseKind: "escalate_holder",
			Evidence: map[string]any{"plan": "Escalate"}},
		"empty proposal": {},
	}
	for name, proposal := range cases {
		if request := custodianRequest(proposal); request.Contract != nil {
			t.Fatalf("%s: typed as %#v", name, request.Contract)
		}
	}
	// A SQL response keeps its SQL-derived contract whatever its kind.
	request := custodianRequest(CustodianProposal{Feature: "freeze",
		ResponseKind: string(freeze.ResponseVacuumFreeze),
		SQL:          `VACUUM (FREEZE) "public"."orders"`})
	if request.Contract == nil || request.Contract.ActionType == "plan_bloat_remediation" {
		t.Fatalf("freeze SQL contract = %#v", request.Contract)
	}
}

// Through the real standing gate the plan parks with the typed high tier:
// SQL validation of the empty plan statement parks it.
func TestBloatPlanParksHighThroughTheStandingGate(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Trust.Level, cfg.Trust.Tier3Safe = "autonomous", true
	exec := New(nil, cfg, zeroTime(), func(string, string, ...any) {})
	exec.SetExecutionMode("auto")
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	exec.EnableStandingPolicyDocument(policy.UnattendedProfile(), nil)
	decision := exec.EvaluateCustodianProposal(context.Background(),
		bloatProposal(freeze.ResponseParkBloat))
	if decision.Decision != PolicyDecisionParked || decision.RiskTier != "high" {
		t.Fatalf("bloat plan decision = %#v, want parked at high", decision)
	}
}
