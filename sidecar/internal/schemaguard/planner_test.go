package schemaguard

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPlanRoutesMissingFKIndexThroughVerifiedIndexLifecycle(t *testing.T) {
	request := testRequest(InvariantMissingFKIndex)
	request.Invariant.ProposedSQL =
		"CREATE INDEX CONCURRENTLY ON public.orders (customer_id)"
	decision, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationVerifiedIndex, DispositionApply)
	if !decision.RequiresVerification || decision.RequiresRehearsal {
		t.Fatalf("FK route lost verify-only contract: %#v", decision)
	}
	if decision.Route != RouteVerifyIndex {
		t.Fatalf("FK route = %q, want %q", decision.Route, RouteVerifyIndex)
	}
}

func TestPlanMissingFKIndexWithoutPolicyGrantDoesNotApply(t *testing.T) {
	request := testRequest(InvariantMissingFKIndex)
	request.Policy.AllowFKIndexApply = false
	decision, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationVerifiedIndex, DispositionRecommend)
}

func TestPlanRetentionFirstRunIsMandatoryDryRun(t *testing.T) {
	decision, err := Plan(context.Background(), retentionRequest())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationRetention, DispositionDryRun)
	if decision.Route != RouteRetention {
		t.Fatalf("retention route = %q, want %q", decision.Route, RouteRetention)
	}
	if decision.MayDeleteData {
		t.Fatalf("first retention run may delete data: %#v", decision)
	}
}

func TestPlanRetentionCanApplyOnlyAfterRecordedDryRun(t *testing.T) {
	request := retentionRequest()
	request.History.SuccessfulRetentionDryRuns = 1
	decision, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationRetention, DispositionApply)
	if !decision.MayDeleteData {
		t.Fatalf("enforcement did not identify destructive effect: %#v", decision)
	}
}

func TestPlanRetentionNeedsDeclaredWindow(t *testing.T) {
	request := retentionRequest()
	request.Contract.RetentionWindow = 0
	decision, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationRetention, DispositionPark)
}

func TestPlanTypeTighteningIsRecommendAndRehearseOnly(t *testing.T) {
	request := testRequest(InvariantTypeTightening)
	request.Invariant.ProposedSQL =
		"ALTER TABLE public.orders ALTER COLUMN status TYPE order_status"
	decision, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationStructural, DispositionRecommend)
	if decision.Route != RouteCloneRehearsal ||
		!decision.RequiresRehearsal || decision.AutoApply {
		t.Fatalf("type tightening escaped recommend/rehearse gate: %#v", decision)
	}
}

func TestPlanRandomUUIDRewriteIsRecommendAndRehearseOnly(t *testing.T) {
	request := testRequest(InvariantRandomUUIDPK)
	request.Contract.ExpectedPrimaryKey = "uuid_v7"
	request.Invariant.ProposedSQL = "ALTER TABLE public.orders ALTER COLUMN id TYPE bigint"
	decision, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationStructural, DispositionRecommend)
	if decision.Route != RouteCloneRehearsal ||
		!decision.RequiresRehearsal || decision.AutoApply {
		t.Fatalf("UUID rewrite escaped recommend/rehearse gate: %#v", decision)
	}
}

func TestTableContractNeverGrantsRetentionAuthority(t *testing.T) {
	request := retentionRequest()
	request.History.SuccessfulRetentionDryRuns = 1
	request.Policy.AllowRetentionApply = false
	decision, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationRetention, DispositionPark)
	if decision.AutoApply || decision.MayDeleteData {
		t.Fatalf("table contract authorized destructive retention: %#v", decision)
	}
}

func TestTableContractExpectedPKNeverAuthorizesRewrite(t *testing.T) {
	request := testRequest(InvariantRandomUUIDPK)
	request.Contract.ExpectedPrimaryKey = "uuid_v7"
	request.Policy = Policy{
		AllowFKIndexApply: true, AllowRetentionApply: true,
		AllowRedundantIndexCleanup: true, OscillationLimit: 3,
	}
	decision, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if decision.Disposition == DispositionApply || decision.AutoApply {
		t.Fatalf("expected-PK declaration authorized rewrite: %#v", decision)
	}
}

func TestTableContractExemptionConstrainsAutomaticRoute(t *testing.T) {
	request := testRequest(InvariantMissingFKIndex)
	request.Contract.Exemptions = []InvariantKind{InvariantMissingFKIndex}
	decision, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationVerifiedIndex, DispositionPark)
}

func TestPlanParksAfterOscillationLimit(t *testing.T) {
	request := testRequest(InvariantMissingFKIndex)
	request.History.ExternalReversions = request.Policy.OscillationLimit
	decision, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationVerifiedIndex, DispositionPark)
	if !strings.Contains(decision.Reason, "oscillation") {
		t.Fatalf("park reason = %q, want oscillation evidence", decision.Reason)
	}
}

func TestPlanDoesNotParkBeforeOscillationLimit(t *testing.T) {
	request := testRequest(InvariantMissingFKIndex)
	request.History.ExternalReversions = request.Policy.OscillationLimit - 1
	decision, err := Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationVerifiedIndex, DispositionApply)
}

func TestPlanCanceledContextFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	decision, err := Plan(ctx, testRequest(InvariantMissingFKIndex))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Plan error = %v, want context canceled", err)
	}
	if decision.Disposition != DispositionPark || decision.AutoApply {
		t.Fatalf("canceled plan did not fail closed: %#v", decision)
	}
}

func TestPlanEmptyTargetFailsClosed(t *testing.T) {
	request := testRequest(InvariantMissingFKIndex)
	request.Invariant.Table = ""
	decision, err := Plan(context.Background(), request)
	if err == nil {
		t.Fatalf("Plan = %#v, want target validation error", decision)
	}
	if decision.Disposition != DispositionPark {
		t.Fatalf("invalid target decision = %#v, want parked", decision)
	}
}
