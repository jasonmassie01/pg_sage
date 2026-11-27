package api

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// A request cancelled before policy explanation sends no statement to the
// monitored database and fails every known candidate closed.
func TestCasesPolicyCancelledRequestRunsNoStatement(t *testing.T) {
	pool, rec, ctx := tracedCasePool(t)
	resetCaseTables(t, ctx)
	mgr := dbCaseManager(t, ctx, pool)
	seedCountFindings(t, ctx, 20)
	inst := mgr.GetInstance("prod")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	all := batchFixtureCases(20)

	rec.Reset()
	enrichCasesActionPolicies(cancelled, all, inst)

	if n := rec.RoundTrips(); n != 0 {
		t.Fatalf("statements after cancel = %d (%v), want none", n,
			rec.Matching(""))
	}
	for _, c := range all {
		for _, candidate := range c.ActionCandidates {
			if candidate.ActionType == "mystery_family" {
				continue
			}
			d := candidate.PolicyDecision
			if d == nil || d.Decision != executor.PolicyDecisionBlocked ||
				d.BlockedReason != string(policy.ReasonPolicyUnavailable) {
				t.Fatalf("%s decision = %#v, want blocked policy_unavailable",
					candidate.ActionType, d)
			}
		}
	}

	rec.Reset()
	if _, err := queryProjectedCases(cancelled, mgr, "prod"); !errors.Is(err, context.Canceled) {
		t.Fatalf("queryProjectedCases(cancelled) error = %v, want context.Canceled", err)
	}
	if n := rec.RoundTrips(); n != 0 {
		t.Fatalf("handler statements after cancel = %d, want none", n)
	}
}
