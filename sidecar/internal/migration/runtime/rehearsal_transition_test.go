package runtime

import (
	"context"
	"testing"
	"time"

	clonepkg "github.com/pg-sage/sidecar/internal/clone"
	planpkg "github.com/pg-sage/sidecar/internal/migration/plan"
	rehearsalpkg "github.com/pg-sage/sidecar/internal/migration/rehearsal"
	"github.com/pg-sage/sidecar/internal/verify"
)

// State transitions (lab-04b, unchanged from migration-06 part A):
// recommend-only without a measured workload, then promote expand, then
// contract pending, asserted through the real rehearsal judge with a
// measured workload. No parse tree needed: the plan is built by hand.

type handPlanner struct{}

func (handPlanner) Plan(_ context.Context, req planpkg.Request) (planpkg.Plan, error) {
	return planpkg.Plan{Cycle: req.Cycle, Table: req.Table, ContractNotBeforeCycle: 2,
		ContractNotBefore: time.Hour,
		ContractGuard: planpkg.ContractGuard{Kind: planpkg.GuardValidUniqueIndex,
			Schema: "public", Table: "users", Name: "users_email_key_idx"},
		ExpandSteps: []planpkg.Step{{Kind: planpkg.StepCreateUniqueIndex,
			Phase: planpkg.PhaseExpand, RequiresTopLevel: true,
			SQL: `CREATE UNIQUE INDEX CONCURRENTLY "users_email_key_idx" ON ` +
				`"public"."users" ("email")`}},
		ContractSteps: []planpkg.Step{{Kind: planpkg.StepAttachUniqueConstraint,
			Phase: planpkg.PhaseContract,
			SQL: `ALTER TABLE "public"."users" ADD CONSTRAINT "users_email_key" ` +
				`UNIQUE USING INDEX "users_email_key_idx"`}},
	}, nil
}

type oneClone struct{}

func (oneClone) SnapshotAge(context.Context) (time.Duration, error) { return time.Minute, nil }
func (oneClone) Create(context.Context, clonepkg.CloneSpec) (clonepkg.Clone, error) {
	return clonepkg.Clone{ID: "clone-7", DSN: "postgres://clone.invalid/app"}, nil
}
func (oneClone) Destroy(context.Context, clonepkg.Clone) error { return nil }
func (oneClone) ReceiptOf(string) (int64, bool)                { return 55, true }

type workloadRunner struct{ m rehearsalpkg.Measurement }

func (r workloadRunner) Run(context.Context, string, planpkg.Plan) (
	rehearsalpkg.Measurement, error) {
	return r.m, nil
}

func applyWith(t *testing.T, m rehearsalpkg.Measurement) (Result, *fixture) {
	t.Helper()
	f := newFixture()
	rehearser := rehearsalpkg.NewOrchestrator(oneClone{}, workloadRunner{m},
		rehearsalpkg.Options{MaxCloneAge: time.Hour, RegressionPct: 15, Provider: "dle"})
	result, err := NewOrchestrator(handPlanner{}, rehearser, f.gate, f.applier,
		f.recorder).Apply(context.Background(), request())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return result, f
}

func TestRehearsalTransitionsWithAMeasuredWorkload(t *testing.T) {
	result, f := applyWith(t, rehearsalpkg.Measurement{
		WorkloadNote: NoStatementReferences})
	if result.Verdict != VerdictRecommendOnly || f.applier.calls != 0 ||
		result.Reason != "inconclusive_no_workload: "+NoStatementReferences ||
		result.Ladder == nil || result.Ladder.Result != verify.LadderInconclusive {
		t.Fatalf("no workload: %#v", result)
	}
	measured := rehearsalpkg.QueryMeasurement{QueryID: 9, BeforeCost: 8.3, AfterCost: 8.3,
		BeforePlanHash: "v2u:a", AfterPlanHash: "v2u:a",
		NotExecuted: "not executed: parameters unknown"}
	result, f = applyWith(t, rehearsalpkg.Measurement{
		AffectedQueries: []rehearsalpkg.QueryMeasurement{measured}})
	if result.Verdict != VerdictExpanded || result.Reason != ReasonContractPending ||
		f.applier.calls != 1 || len(result.PendingContractSQL) != 1 ||
		result.ContractScript == "" {
		t.Fatalf("measured workload: %#v applies=%d", result, f.applier.calls)
	}
	if len(result.AffectedQueries) != 1 || result.AffectedQueries[0].QueryID != 9 ||
		result.Ladder == nil || !result.Ladder.Passed() ||
		result.Ladder.Evidence["receipt_id"] != int64(55) {
		t.Fatalf("evidence: queries=%#v ladder=%#v", result.AffectedQueries, result.Ladder)
	}
	last := f.recorder.last
	if last.CloneReceipt != 55 || last.Ladder == nil || !last.Ladder.Passed() ||
		last.ContractDelay != time.Hour || len(last.Measurement.AffectedQueries) != 1 {
		t.Fatalf("recorded = %#v", last)
	}
}

// A parked rehearsal records its failed ladder and applies nothing.
func TestRehearsalRegressionParksWithFailedLadder(t *testing.T) {
	result, f := applyWith(t, rehearsalpkg.Measurement{
		AffectedQueries: []rehearsalpkg.QueryMeasurement{{QueryID: 9, BeforeCost: 8.3,
			AfterCost: 4180, BeforePlanHash: "v2u:index", AfterPlanHash: "v2u:seq",
			NotExecuted: "not executed: parameters unknown"}}})
	if result.Verdict != VerdictParked || f.applier.calls != 0 || f.gate.calls != 0 ||
		result.Ladder == nil || result.Ladder.Result != verify.LadderFailed ||
		f.recorder.last.CloneReceipt != 55 {
		t.Fatalf("result = %#v", result)
	}
}
