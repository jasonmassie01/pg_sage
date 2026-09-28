package schemaguard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// D5: retention deletes act only on the owner-declared column. A contract
// without a usable declared column parks with pg_sage's suggestion as text.
// No concurrent access tests: Plan is a pure function and the custodian
// fixtures below are local to each test.

func TestPlanRetentionParksWhenColumnNotDeclared(t *testing.T) {
	request := retentionRequest()
	request.Contract.RetentionColumn = ""
	request.Invariant.RetentionColumn = ""
	request.Invariant.RetentionSuggestion = "created_at"
	request.History.SuccessfulRetentionDryRuns = 1

	decision, err := Plan(context.Background(), request)

	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationRetention, DispositionPark)
	if decision.MayDeleteData || decision.AutoApply {
		t.Fatalf("undeclared column may delete data: %#v", decision)
	}
	for _, want := range []string{
		"retention column not declared", "suggested: created_at", "retention.column",
	} {
		if !strings.Contains(decision.Reason, want) {
			t.Fatalf("park reason %q lacks %q", decision.Reason, want)
		}
	}
	if shouldRoute(decision) {
		t.Fatalf("undeclared retention column was routed: %#v", decision)
	}
}

func TestPlanRetentionParksWithoutSuggestionWhenNoCandidateColumn(t *testing.T) {
	request := retentionRequest()
	request.Contract.RetentionColumn = ""
	request.Invariant.RetentionColumn = ""

	decision, err := Plan(context.Background(), request)

	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationRetention, DispositionPark)
	if !strings.Contains(decision.Reason, "suggested: none") {
		t.Fatalf("park reason %q must say no column could be suggested", decision.Reason)
	}
}

// The detector leaves Invariant.RetentionColumn empty when the declared
// column is missing, dropped or not timestamptz/timestamp/date.
func TestPlanRetentionParksWhenDeclaredColumnUnusable(t *testing.T) {
	request := retentionRequest()
	request.Contract.RetentionColumn = "ingested_at"
	request.Invariant.RetentionColumn = ""
	request.Invariant.RetentionSuggestion = "created_at"
	request.History.SuccessfulRetentionDryRuns = 1

	decision, err := Plan(context.Background(), request)

	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationRetention, DispositionPark)
	for _, want := range []string{`"ingested_at"`, "suggested: created_at"} {
		if !strings.Contains(decision.Reason, want) {
			t.Fatalf("park reason %q lacks %q", decision.Reason, want)
		}
	}
}

// A detector/contract race (column in the invariant differs from the
// declaration read afterwards) must park, never delete by either name.
func TestPlanRetentionParksWhenInvariantColumnDiffersFromContract(t *testing.T) {
	request := retentionRequest()
	request.Contract.RetentionColumn = "ingested_at"
	request.Invariant.RetentionColumn = "created_at"
	request.History.SuccessfulRetentionDryRuns = 1

	decision, err := Plan(context.Background(), request)

	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationRetention, DispositionPark)
	if decision.MayDeleteData {
		t.Fatalf("mismatched column may delete data: %#v", decision)
	}
}

func TestPlanRetentionWithDeclaredColumnStillDryRunsFirst(t *testing.T) {
	decision, err := Plan(context.Background(), retentionRequest())

	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	requirePlan(t, decision, RemediationRetention, DispositionDryRun)
	if decision.Reason != "" {
		t.Fatalf("declared dry run carries park reason %q", decision.Reason)
	}
}

// Memo scenario 9: one undeclared retention contract must not stop the scan
// before later invariants are planned and routed.
func TestScanContinuesAfterUndeclaredRetentionColumn(t *testing.T) {
	detector := &fakeDetector{items: []Invariant{
		{Kind: InvariantUnboundedAppend, Schema: "public", Table: "events",
			RetentionSuggestion: "created_at"},
		{Kind: InvariantMissingFKIndex, Schema: "public", Table: "orders",
			ProposedSQL: "CREATE INDEX CONCURRENTLY orders_customer_idx ON orders(customer_id)"},
	}}
	contracts := &fakeContractSource{contract: TableContract{
		AppendOnly: true, RetentionWindow: 30 * 24 * time.Hour,
	}}
	router := &retentionRefusingRouter{}
	records := &fakeDecisionRecorder{}
	custodian := NewCustodian(detector, contracts, &fakeHistorySource{}, router,
		records, testPolicy())

	result, err := custodian.Scan(context.Background())

	if err != nil {
		t.Fatalf("Scan aborted: %v", err)
	}
	if result.Detected != 2 || result.Recorded != 2 || result.Routed != 1 {
		t.Fatalf("result=%+v, want 2 detected, 2 recorded, 1 routed", result)
	}
	if len(router.routed) != 1 || router.routed[0].Invariant.Kind != InvariantMissingFKIndex {
		t.Fatalf("routed=%+v, want only the FK invariant", router.routed)
	}
	park := records.items[0]
	if park.Decision.Disposition != DispositionPark ||
		!strings.Contains(park.Decision.Reason, "suggested: created_at") {
		t.Fatalf("retention record=%+v, want park naming the suggestion", park.Decision)
	}
}

// retentionRefusingRouter fails retention routes the way the PostgreSQL
// enforcer does for an unusable column, and records every other route.
type retentionRefusingRouter struct{ routed []Remediation }

func (r *retentionRefusingRouter) Route(_ context.Context, item Remediation) error {
	if item.Decision.Route == RouteRetention {
		return errors.New("retention enforcement requires a time column and window")
	}
	r.routed = append(r.routed, item)
	return nil
}
