package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// vacuum-04: a plan-only proposal the standing gate could not evaluate is
// an error the autonomy loop logs, not a silent success.
// No concurrency test: the router is stateless; the loop calls it serially.

type verdictGate struct {
	decision policy.Decision
	request  policy.ActionRequest
	calls    int
}

func (g *verdictGate) Authorize(_ context.Context, req policy.ActionRequest) policy.Decision {
	g.calls++
	g.request = req
	return g.decision
}

func bloatPlanProposal() autonomy.Proposal {
	return autonomy.Proposal{Database: "orders", Feature: "bloat_remediation",
		ResponseKind: "park_bloat", Plan: "Park bloat remediation",
		TargetObjects: []string{"public.orders"},
		Evidence:      map[string]any{"plan": "Park bloat remediation", "bloat_ratio": 0.7}}
}

func routerWithGate(gate policy.Gate) executorProposalRouter {
	exec := executor.New(nil, config.DefaultConfig(), time.Now(), nil)
	if gate != nil {
		exec.WithPolicyGate(gate)
	}
	return executorProposalRouter{executor: exec,
		isReplica: func(context.Context) bool { return false }}
}

func TestRoutePlanOnlyPolicyUnavailableIsAnError(t *testing.T) {
	gate := &verdictGate{decision: policy.Decision{Verdict: policy.VerdictBlocked,
		Reason: policy.ReasonPolicyUnavailable, Detail: "ledger insert failed: 23514"}}
	err := routerWithGate(gate).Route(context.Background(), bloatPlanProposal())
	if !errors.Is(err, errCustodianPolicyUnavailable) || gate.calls != 1 {
		t.Fatalf("Route = %v after %d evaluations, want errCustodianPolicyUnavailable",
			err, gate.calls)
	}
	for _, want := range []string{"bloat_remediation", "public.orders", "23514"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

func TestRoutePlanOnlyWithoutAGateIsAnError(t *testing.T) {
	err := routerWithGate(nil).Route(context.Background(), bloatPlanProposal())
	if !errors.Is(err, errCustodianPolicyUnavailable) {
		t.Fatalf("Route without a standing gate = %v, want errCustodianPolicyUnavailable", err)
	}
}

func TestRoutePlanOnlyRecordedVerdictsAreNotErrors(t *testing.T) {
	for _, decision := range []policy.Decision{
		{Verdict: policy.VerdictPark, Reason: policy.ReasonNoTypedContract,
			RiskTier: policy.RiskHigh},
		{Verdict: policy.VerdictBlocked, Reason: policy.ReasonReplicaMutation,
			RiskTier: policy.RiskHigh},
		{Verdict: policy.VerdictObserveOnly, Reason: policy.ReasonObserveOnly},
	} {
		gate := &verdictGate{decision: decision}
		if err := routerWithGate(gate).Route(context.Background(),
			bloatPlanProposal()); err != nil {
			t.Fatalf("recorded verdict %s/%s returned %v", decision.Verdict, decision.Reason, err)
		}
	}
}

func TestRoutePlanOnlyCarriesTheTypedBloatContract(t *testing.T) {
	gate := &verdictGate{decision: policy.Decision{Verdict: policy.VerdictPark,
		Reason: policy.ReasonNoTypedContract, RiskTier: policy.RiskHigh}}
	if err := routerWithGate(gate).Route(context.Background(), bloatPlanProposal()); err != nil {
		t.Fatalf("Route: %v", err)
	}
	contract := gate.request.Contract
	if contract == nil || contract.ActionType != "plan_bloat_remediation" ||
		contract.RiskTier != policy.RiskHigh || gate.request.Feature != "bloat_remediation" ||
		gate.request.SQL != "" || gate.request.IncidentFamily != "" {
		t.Fatalf("routed bloat request = %#v (contract %#v)", gate.request, contract)
	}
}
