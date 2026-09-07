package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

type recordingStandingGate struct {
	request policy.ActionRequest
	result  policy.Decision
}

func (g *recordingStandingGate) Authorize(
	_ context.Context, request policy.ActionRequest,
) policy.Decision {
	g.request = request
	return g.result
}

func TestExecutorUsesStandingPolicyAsAuthorizationPath(t *testing.T) {
	gate := &recordingStandingGate{result: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskSafe,
		Reason: policy.ReasonAuthorized, EvidenceID: "ev-1", DecisionID: 41,
	}}
	exec := New(nil, &config.Config{}, nil, time.Time{}, func(string, string, ...any) {})
	exec.WithPolicyGate(gate)
	finding := analyzer.Finding{
		Category: "stale_statistics", ObjectIdentifier: "public.orders",
		RecommendedSQL: "ANALYZE public.orders",
	}

	decision := exec.evaluateFindingPolicy(context.Background(), finding, false)

	if decision.Decision != PolicyDecisionExecute || decision.DecisionID != 41 {
		t.Fatalf("decision = %#v", decision)
	}
	if gate.request.Feature != "analyze" || gate.request.SQL != finding.RecommendedSQL {
		t.Fatalf("request = %#v", gate.request)
	}
	if len(gate.request.TargetObjs) != 1 || gate.request.TargetObjs[0] != "public.orders" {
		t.Fatalf("target objects = %#v", gate.request.TargetObjs)
	}
}

func TestExecutorStandingPolicyParksUnknownContract(t *testing.T) {
	gate := &recordingStandingGate{result: policy.Decision{
		Verdict: policy.VerdictPark, Reason: policy.ReasonNoTypedContract,
	}}
	exec := New(nil, &config.Config{}, nil, time.Time{}, func(string, string, ...any) {})
	exec.WithPolicyGate(gate)

	decision := exec.evaluateFindingPolicy(context.Background(), analyzer.Finding{
		ObjectIdentifier: "public.orders", RecommendedSQL: "TRUNCATE public.orders",
	}, false)

	if decision.Decision != PolicyDecisionParked || gate.request.Contract != nil {
		t.Fatalf("decision=%#v request=%#v", decision, gate.request)
	}
}

func TestLedgerInputPreservesCustodianEvidence(t *testing.T) {
	input := ledgerInput(nil, 1, policy.ActionRequest{
		Feature: "freeze", Evidence: map[string]any{
			"pid": 42, "xmin_age": int64(900),
		},
	}, policy.Decision{Verdict: policy.VerdictPark})
	if input.Evidence["pid"] != 42 || input.Evidence["xmin_age"] != int64(900) {
		t.Fatalf("custodian evidence = %#v", input.Evidence)
	}
}
