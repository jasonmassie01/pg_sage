package earned

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// The limiter answers self-initiated requests from the same ledger: the
// granted level of (database, trust family, class), capped by the class
// and the contract, and by L1 while a database-wide downgrade signal
// (error budget, HA role) or a concurrent action holds. A grandfathered
// or earned self-initiated level is not re-derived from evidence at every
// authorization: it changes only by an approved promotion or a recorded
// demotion. The stale-evidence signal belongs to incident snapshots.

func selfVacuum(target string) policy.ActionRequest {
	return policy.ActionRequest{SQL: "VACUUM " + target, TargetObjs: []string{target},
		Contract: &policy.ActionContract{ActionType: "vacuum_table", RiskTier: policy.RiskSafe,
			RollbackClass: policy.RollbackNoRollbackNeeded}}
}

func TestLimiterSelfInitiatedUsesTheLedger(t *testing.T) {
	lf := newLimiterFixture(t)
	got, err := lf.lim.Limit(lf.ctx, selfVacuum("public.orders"))
	if err != nil || got.Level != 1 || got.Granted != 1 || got.Family != "hygiene" ||
		got.Class != "vacuum" || got.Downgraded {
		t.Fatalf("default self level = %+v (%v)", got, err)
	}
	if _, err := lf.svc.SeedGrandfathered(lf.ctx, lf.db,
		rampBound(lf.clock.Now(), 60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err = lf.lim.Limit(lf.ctx, selfVacuum("public.orders"))
	if err != nil || got.Level != 3 || got.Granted != 3 || got.Downgraded {
		t.Fatalf("grandfathered self level = %+v (%v)", got, err)
	}
	if lf.budget.calls == 0 {
		t.Fatal("the error budget was not read for an L3 self-initiated request")
	}
}

func TestLimiterSelfInitiatedDowngradeSignals(t *testing.T) {
	lf := newLimiterFixture(t)
	if _, err := lf.svc.SeedGrandfathered(lf.ctx, lf.db,
		rampBound(lf.clock.Now(), 60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	lf.budget.state = BudgetState{Configured: true, FastBurning: true, Detail: "burning"}
	got, err := lf.lim.Limit(lf.ctx, selfVacuum("public.orders"))
	if err != nil || got.Level != 1 || !got.Downgraded ||
		!strings.Contains(strings.Join(got.Reasons, ","), DowngradeBudgetBurn) {
		t.Fatalf("budget burn = %+v (%v)", got, err)
	}
	lf.budget.state = BudgetState{Configured: true}
	lf.conc.count = 2
	got, _ = lf.lim.Limit(lf.ctx, selfVacuum("public.orders"))
	if got.Level != 1 || !strings.Contains(strings.Join(got.Reasons, ","),
		DowngradeConcurrent) {
		t.Fatalf("concurrent action = %+v", got)
	}
	lf.conc.count = 0
	lf.ha.state = HAState{Role: RoleReplica}
	got, _ = lf.lim.Limit(lf.ctx, selfVacuum("public.orders"))
	if got.Level != 1 || !got.Downgraded {
		t.Fatalf("replica = %+v", got)
	}
}

// A finding carries no observation time (the executor re-reads the
// finding's current evidence before the gate): no stale-evidence cap.
func TestLimiterSelfInitiatedIgnoresEvidenceAge(t *testing.T) {
	lf := newLimiterFixture(t)
	if _, err := lf.svc.SeedGrandfathered(lf.ctx, lf.db,
		rampBound(lf.clock.Now(), 60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := lf.lim.Limit(lf.ctx, selfVacuum("public.orders"))
	if err != nil || got.Level != 3 {
		t.Fatalf("self request without an observation time = %+v (%v)", got, err)
	}
	for _, r := range got.Reasons {
		if r == DowngradeStaleEvidence {
			t.Fatal("stale-evidence cap applied to a self-initiated request")
		}
	}
}

// A regression in the tuning family does not cap hygiene, nor even the
// tuning family's other classes: self-initiated demerits demote the
// class one level instead of a family-wide safety window.
func TestLimiterSelfInitiatedHasNoFamilySafetyCap(t *testing.T) {
	lf := newLimiterFixture(t)
	if _, err := lf.svc.SeedGrandfathered(lf.ctx, lf.db,
		rampBound(lf.clock.Now(), 60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := lf.svc.RecordOutcome(lf.ctx, Outcome{Database: lf.db, Family: FamilyTuning,
		Class: ClassIndexCreate, Level: L3, Result: ResultHarmful, Source: SourceOperator,
		Actor: "user:1:a@b"}); err != nil {
		t.Fatal(err)
	}
	req := selfReq("apply_query_hint", "INSERT INTO hint_plan.hints VALUES (1)")
	got, err := lf.lim.Limit(lf.ctx, req)
	if err != nil || got.Level != 3 || got.Downgraded {
		t.Fatalf("query_hint after an index_create harm = %+v (%v)", got, err)
	}
	if st, _ := lf.svc.Granted(lf.ctx, FamilyTuning, ClassIndexCreate); st.Level != L2 {
		t.Fatalf("a harmful self outcome demoted index_create to %v, want L2", st.Level)
	}
	if st, _ := lf.svc.Granted(lf.ctx, FamilyTuning, ClassConfigGUC); st.Level != L3 {
		t.Fatalf("a harmful index_create demoted config_guc to %v", st.Level)
	}
}

func TestLimiterGovernsOnlyTrustFamilies(t *testing.T) {
	lf := newLimiterFixture(t)
	if !lf.lim.Governs(selfVacuum("public.orders")) {
		t.Fatal("the limiter does not govern a self-initiated vacuum")
	}
	if lf.lim.Governs(selfReq("alter_table", "ALTER TABLE t ADD COLUMN c int")) {
		t.Fatal("the limiter governs a schema change")
	}
	var scope policy.AutonomyScope = lf.lim
	_ = scope
}

func TestLimiterSelfInitiatedContractCap(t *testing.T) {
	lf := newLimiterFixture(t)
	if _, err := lf.svc.SeedGrandfathered(lf.ctx, lf.db,
		rampBound(lf.clock.Now(), 60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := selfVacuum("public.orders")
	req.Contract.RollbackClass = policy.RollbackNotReversible
	got, err := lf.lim.Limit(lf.ctx, req)
	if err != nil || got.Level != 1 {
		t.Fatalf("irreversible contract at grandfathered L3 = %+v (%v)", got, err)
	}
}

func TestLimiterSelfInitiatedReadFailureIsAnError(t *testing.T) {
	lf := newLimiterFixture(t)
	other := lf.svc.Limiter(Binding{Database: "elsewhere"})
	if _, err := other.Limit(lf.ctx, selfVacuum("public.orders")); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("a limiter bound to another database answered: %v", err)
	}
}
