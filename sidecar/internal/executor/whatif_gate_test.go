package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Phase 0 item 7: an optimizer index whose HypoPG what-if was not
// measured end to end ("unverified", or no verdict at all) must not pass
// an autonomous gate: it carries the approval-required guardrail, so the
// standing gate queues it for an operator instead of executing it.

func optimizerFinding(verdict any) analyzer.Finding {
	f := analyzer.Finding{Category: "missing_index", ObjectType: "index",
		ObjectIdentifier: "public.orders|btree(status)",
		RecommendedSQL:   "CREATE INDEX CONCURRENTLY idx_orders_status ON public.orders (status)",
		RollbackSQL:      `DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_orders_status"`,
		ActionRisk:       "moderate", Detail: map[string]any{}}
	if verdict != nil {
		f.Detail["what_if_verdict"] = verdict
	}
	return f
}

func hasApprovalGuardrail(req policy.ActionRequest) bool {
	if req.Contract == nil {
		return false
	}
	for _, g := range req.Contract.Guardrails {
		if g == policy.GuardrailApprovalRequired {
			return true
		}
	}
	return false
}

func TestFindingRequest_UnverifiedIndexNeedsApproval(t *testing.T) {
	cases := []struct {
		name     string
		verdict  any
		approval bool
	}{
		{"verified", "verified", false},
		{"unverified", "unverified", true},
		{"missing verdict (legacy row)", nil, true},
		{"non-string verdict", true, true},
		{"unknown verdict", "maybe", true},
	}
	for _, c := range cases {
		req := findingRequest(optimizerFinding(c.verdict), false)
		if req.Contract == nil || req.Contract.ActionType != "create_index_concurrently" {
			t.Fatalf("%s: contract = %+v", c.name, req.Contract)
		}
		if got := hasApprovalGuardrail(req); got != c.approval {
			t.Errorf("%s: approval guardrail = %t, want %t", c.name, got, c.approval)
		}
	}
}

// Only optimizer findings are gated on a what-if verdict.
func TestFindingRequest_OtherCategoriesUnaffected(t *testing.T) {
	f := optimizerFinding(nil)
	f.Category = "missing_fk_index"
	if hasApprovalGuardrail(findingRequest(f, false)) {
		t.Fatal("non-optimizer CREATE INDEX gained the what-if guardrail")
	}
	f = analyzer.Finding{Category: "missing_index", RecommendedSQL: "ANALYZE public.orders"}
	if req := findingRequest(f, false); hasApprovalGuardrail(req) {
		t.Fatal("non-index SQL gained the what-if guardrail")
	}
}

// The shared contract catalog is not mutated by the per-request guardrail.
func TestFindingRequest_DoesNotMutateContractCatalog(t *testing.T) {
	_ = findingRequest(optimizerFinding("unverified"), false)
	contract, ok := ContractForActionType("create_index_concurrently")
	if !ok {
		t.Fatal("no create_index_concurrently contract")
	}
	if hasApprovalGuardrail(policy.ActionRequest{Contract: policyContract(contract)}) {
		t.Fatal("catalog contract now requires approval for every index")
	}
}

// End to end through the standing gate at full autonomy: verified
// executes, unverified queues for approval.
func TestStandingGate_UnverifiedIndexQueuesApproval(t *testing.T) {
	runtime := matrixRuntime("autonomous")
	gate := policy.NewGate(policy.GateConfig{
		Runtime: func(context.Context, policy.ActionRequest) (policy.RuntimeState, error) {
			return runtime, nil
		},
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return policy.UnattendedProfile(), nil
		},
		ValidateSQL: func(string) error { return nil },
		Now:         func() time.Time { return matrixNow },
	})
	verified := gate.Authorize(context.Background(),
		findingRequest(optimizerFinding("verified"), false))
	if verified.Verdict != policy.VerdictExecute {
		t.Fatalf("verified index = %+v, want execute", verified)
	}
	unverified := gate.Authorize(context.Background(),
		findingRequest(optimizerFinding("unverified"), false))
	if unverified.Verdict != policy.VerdictQueueApproval ||
		unverified.Reason != policy.ReasonApprovalRequired {
		t.Fatalf("unverified index = %+v, want queue_approval", unverified)
	}
}
