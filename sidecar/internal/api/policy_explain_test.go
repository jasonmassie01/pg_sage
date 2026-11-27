package api

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/executor"
)

// policy-07 review: explainedFrom words a candidate's verdict. The gate's
// detail belongs to the gate's reason: it is shown with that reason, and
// dropped when a candidate-level refusal (an unknown type, a provider
// that manages the setting) is the reason served.

func TestExplainedFromShowsTheGateDetailWithItsReason(t *testing.T) {
	candidate := cases.ActionCandidate{ActionType: "create_index",
		BlockedReason: "rate_limit_exceeded",
		PolicyDecision: &cases.ActionPolicyDecision{Decision: "blocked", RiskTier: "safe",
			BlockedReason: "rate_limit_exceeded", Guardrails: []string{"lock_timeout"}}}
	got := explainedFrom(candidate, "next frees at 18:00")
	if got.Decision != "blocked" || got.BlockedReason != "rate_limit_exceeded" ||
		got.Detail != "next frees at 18:00" ||
		got.HumanReason != "rate limit exceeded (next frees at 18:00)" {
		t.Fatalf("explained = %+v, want the gate's reason with its detail", got)
	}
	if len(got.Guardrails) != 1 || got.Guardrails[0] != "lock_timeout" {
		t.Fatalf("guardrails = %v, want the gate's", got.Guardrails)
	}
}

func TestExplainedFromDropsTheGateDetailWhenTheCandidateRefusalWins(t *testing.T) {
	candidate := cases.ActionCandidate{ActionType: "alter_system_guc",
		BlockedReason: "lakebase manages this setting",
		PolicyDecision: &cases.ActionPolicyDecision{Decision: "blocked",
			BlockedReason: "provider_managed_setting"}}
	got := explainedFrom(candidate, "next frees at 18:00")
	if got.BlockedReason != "lakebase manages this setting" || got.Detail != "" ||
		got.HumanReason != "lakebase manages this setting" {
		t.Fatalf("explained = %+v, want the provider refusal without the gate's detail", got)
	}
}

// Nil: a candidate the gate never saw (an unknown type) is blocked with
// its own reason and an empty, non-nil guardrail list.
func TestExplainedFromWithoutAGateDecision(t *testing.T) {
	candidate := cases.ActionCandidate{ActionType: "mystery", RiskTier: "high",
		BlockedReason: reasonUnknownActionType}
	got := explainedFrom(candidate, "")
	if got.Decision != executor.PolicyDecisionBlocked || got.RiskTier != "high" ||
		got.HumanReason != reasonUnknownActionType || got.Guardrails == nil ||
		len(got.Guardrails) != 0 {
		t.Fatalf("explained = %+v, want blocked by the candidate's own reason", got)
	}
}
