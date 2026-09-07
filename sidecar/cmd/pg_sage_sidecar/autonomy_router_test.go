package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

func TestAutonomyRuntimeRouterPreservesStandingAuthority(t *testing.T) {
	gate := &runtimePolicyRecorder{}
	exec := executor.New(nil, config.DefaultConfig(), nil, time.Now(), nil)
	exec.WithPolicyGate(gate)
	router := executorProposalRouter{executor: exec}
	proposal := autonomy.Proposal{Database: "fixture", Feature: "fk_index",
		SQL:           "CREATE INDEX CONCURRENTLY fixture_idx ON public.orders(customer_id)",
		TargetObjects: []string{"public.orders"}}
	ctx := context.Background()
	err := router.Route(ctx, proposal)
	if !errors.Is(err, executor.ErrCustodianProposalWithheld) || gate.calls != 1 ||
		gate.request.SQL != proposal.SQL || len(gate.request.TargetObjs) != 1 ||
		gate.request.TargetObjs[0] != "public.orders" {
		t.Fatalf("proposal lost target/authority: calls=%d err=%v", gate.calls, err)
	}
	err = router.RouteVerifiedIndex(ctx, proposal, "DROP INDEX fixture_idx", []int64{41})
	if !errors.Is(err, executor.ErrCustodianProposalWithheld) || gate.calls != 2 {
		t.Fatalf("verified route bypassed standing gate: calls=%d err=%v", gate.calls, err)
	}
	proposal.SQL = ""
	if err := router.Route(ctx, proposal); err != nil || gate.calls != 3 || gate.request.SQL != "" {
		t.Fatalf("observation route did not record policy evaluation: calls=%d err=%v", gate.calls, err)
	}
}

type runtimePolicyRecorder struct {
	calls   int
	request policy.ActionRequest
}

func (r *runtimePolicyRecorder) Authorize(
	_ context.Context, request policy.ActionRequest,
) policy.Decision {
	r.calls++
	r.request = request
	return policy.Decision{Verdict: policy.VerdictBlocked, Reason: policy.ReasonPolicyUnavailable}
}
