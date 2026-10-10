package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

func TestGuardRoleContracts_TypedPerSpec(t *testing.T) {
	cases := []struct {
		actionType, risk, rollback string
	}{
		{ActionTypeGuardRoleEnsure, "moderate", "reversible"},
		{ActionTypeGuardRoleRetire, "high", "not_reversible"},
	}
	for _, c := range cases {
		contract, ok := ContractForActionType(c.actionType)
		if !ok {
			t.Fatalf("%s has no contract", c.actionType)
		}
		if err := contract.Validate(); err != nil {
			t.Fatalf("%s: %v", c.actionType, err)
		}
		if contract.BaseRiskTier != c.risk || contract.RollbackClass != c.rollback {
			t.Fatalf("%s: risk %s rollback %s", c.actionType, contract.BaseRiskTier,
				contract.RollbackClass)
		}
		if got := changeClassForActionType(c.actionType); got != "agent_access" {
			t.Fatalf("%s change class = %q", c.actionType, got)
		}
		pc, ok := PolicyContractFor(c.actionType)
		if !ok || pc.ActionType != c.actionType || string(pc.RiskTier) != c.risk ||
			string(pc.RollbackClass) != c.rollback {
			t.Fatalf("%s policy contract = %+v", c.actionType, pc)
		}
		if len(pc.Guardrails) != 0 {
			t.Fatalf("%s carries gate guardrails %v", c.actionType, pc.Guardrails)
		}
		for _, provider := range []string{"postgres", "rds", "aurora", "cloud-sql",
			"alloydb", "azure", "supabase", "neon"} {
			if !hasProvider(pc.ProviderSupport, provider) {
				t.Fatalf("%s does not support %s", c.actionType, provider)
			}
		}
	}
	if _, ok := PolicyContractFor("guard_role_superuser"); ok {
		t.Fatal("an unknown action type has a contract")
	}
}

func hasProvider(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

type typedTestGate struct{ decision policy.Decision }

func (g typedTestGate) Authorize(context.Context, policy.ActionRequest) policy.Decision {
	return g.decision
}

func TestAuthorizeTyped(t *testing.T) {
	ctx := context.Background()
	req := policy.ActionRequest{}
	d, err := AuthorizeTyped(ctx, nil, req, false)
	var withheld *WithheldError
	if !errors.As(err, &withheld) || withheld.Reauthorized ||
		d.BlockedReason != reasonNoStandingPolicy {
		t.Fatalf("no gate: %+v %v", d, err)
	}
	gate := typedTestGate{policy.Decision{Verdict: policy.VerdictExecute,
		Reason: policy.ReasonOperatorApproved, DecisionID: 42}}
	d, err = AuthorizeTyped(ctx, gate, req, true)
	if err != nil || d.Decision != PolicyDecisionExecute || d.DecisionID != 42 {
		t.Fatalf("execute: %+v %v", d, err)
	}
	gate = typedTestGate{policy.Decision{Verdict: policy.VerdictObserveOnly,
		Reason: policy.ReasonObserveOnly}}
	d, err = AuthorizeTyped(ctx, gate, req, true)
	if !errors.As(err, &withheld) || !withheld.Reauthorized ||
		d.BlockedReason != "observe_only" || !errors.Is(err, ErrActionWithheld) {
		t.Fatalf("observe only: %+v %v", d, err)
	}
}
