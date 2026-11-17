package connections

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// idleInputs is one role whose longest idle in transaction was age seconds.
func idleInputs(role string, age float64, fact budget.RoleFact) Inputs {
	return Inputs{Database: "shop", SelfRole: "sage",
		IdleInTx: []IdleInTx{{Role: role, AgeSeconds: age, At: t0}},
		Roles:    map[string]budget.RoleFact{role: fact}}
}

// Happy path: a role idle in a transaction for 15 minutes gets a 10
// minute idle_in_transaction_session_timeout in this database, rolled back
// by RESET (it had none).
func TestBuild_IdleInTransactionOverTenMinutesSetsARoleTimeout(t *testing.T) {
	cands, _ := build(t, plainPack(t), idleInputs("app_rw", 900, plainRole()))
	c := only(t, cands, KindIdleInTxTimeout)
	if c.Target != "role:app_rw" || c.ActionType != ActionRoleTimeout ||
		c.Approval != candidate.ApprovalRequired {
		t.Fatalf("timeout %+v", c)
	}
	wantFwd := `ALTER ROLE "app_rw" IN DATABASE "shop" SET ` +
		`idle_in_transaction_session_timeout = '600000ms'`
	wantBack := `ALTER ROLE "app_rw" IN DATABASE "shop" RESET ` +
		`idle_in_transaction_session_timeout`
	if c.ForwardSQL != wantFwd || c.RollbackSQL != wantBack {
		t.Fatalf("SQL %q / %q", c.ForwardSQL, c.RollbackSQL)
	}
	q := c.Prediction.Quantities
	if q[QuantityIdleAge] != 900 || q[QuantityTimeoutSeconds] != 600 {
		t.Fatalf("quantities %v", q)
	}
	if len(c.EvidenceIDs) != 1 || c.EvidenceIDs[0] != hourID(t0Hour) {
		t.Fatalf("evidence %v", c.EvidenceIDs)
	}
}

// Boundary: exactly 10 minutes is not above 10 minutes.
func TestBuild_TenMinutesIdleInTransactionIsNotAboveTheLine(t *testing.T) {
	for _, tc := range []struct {
		age   float64
		steps int
	}{{600, 0}, {600.5, 1}} {
		cands, _ := build(t, plainPack(t), idleInputs("app_rw", tc.age, plainRole()))
		if got := len(ofKind(cands, KindIdleInTxTimeout)); got != tc.steps {
			t.Errorf("age %vs: %d steps, want %d", tc.age, got, tc.steps)
		}
	}
}

// A role whose own timeout is already 10 minutes or less is left alone; a
// looser one is tightened and rolls back to it.
func TestBuild_ExistingRoleTimeout(t *testing.T) {
	for _, tc := range []struct {
		prior, rollback string
	}{{"5min", ""}, {"600000", ""}, {"1h", `ALTER ROLE "app_rw" IN DATABASE "shop" SET ` +
		`idle_in_transaction_session_timeout = '1h'`}} {
		fact := budget.RoleFact{ConnLimit: -1,
			Settings: map[string]string{budget.SettingIdleInTx: tc.prior}}
		cands, _ := build(t, plainPack(t), idleInputs("app_rw", 3600, fact))
		got := ofKind(cands, KindIdleInTxTimeout)
		switch {
		case tc.rollback == "" && len(got) != 0:
			t.Errorf("prior %s: proposed %+v", tc.prior, got)
		case tc.rollback != "" && (len(got) != 1 || got[0].RollbackSQL != tc.rollback):
			t.Errorf("prior %s: %+v, want rollback %q", tc.prior, got, tc.rollback)
		}
	}
}

// A prior value pg_sage cannot set back exactly makes the timeout advice:
// it fails closed rather than lose the role's setting on rollback.
func TestBuild_UnreadablePriorTimeoutIsAdvice(t *testing.T) {
	fact := budget.RoleFact{ConnLimit: -1,
		Settings: map[string]string{budget.SettingIdleInTx: "1d"}}
	cands, _ := build(t, plainPack(t), idleInputs("app_rw", 3600, fact))
	c := only(t, cands, KindIdleInTxTimeout)
	if c.ActionType != candidate.ActionAdvice || c.ForwardSQL != "" ||
		!strings.Contains(c.Reason, "1d") {
		t.Fatalf("unreadable prior: %+v", c)
	}
}

// Error propagation: a superuser is refused by the contract and named.
func TestBuild_SuperuserGetsNoTimeoutAndIsNamed(t *testing.T) {
	fact := budget.RoleFact{Superuser: true, ConnLimit: -1}
	cands, cov := build(t, plainPack(t), idleInputs("ops", 3600, fact))
	if len(ofKind(cands, KindIdleInTxTimeout)) != 0 || !hasNote(cov, "role ops", "superuser") {
		t.Fatalf("superuser: %+v, notes %v", cands, cov.Notes)
	}
}

// A role that no longer exists is noted, not proposed.
func TestBuild_DroppedRoleIsNoted(t *testing.T) {
	in := idleInputs("gone", 3600, plainRole())
	in.Roles = map[string]budget.RoleFact{}
	cands, cov := build(t, plainPack(t), in)
	if len(cands) != 0 || !hasNote(cov, "role gone", "no longer exists") {
		t.Fatalf("dropped role: %+v, notes %v", cands, cov.Notes)
	}
}
