package earned

import (
	"errors"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/policy"
)

// ux-06b integration: a ledger on a real PostgreSQL server, read as the
// Trust view, annotated by the limiter and then by ModeOf, gives each
// class its sentence; lowering the trust ceiling changes it on the next
// read; two admins approving one promotion get one level change.

func modeView(t *testing.T, f *fixture, ceil Ceiling) TrustView {
	t.Helper()
	v, err := f.svc.TrustView(f.ctx)
	if err != nil {
		t.Fatalf("trust view: %v", err)
	}
	// A healthy primary inside its error budget: no downgrade signal holds.
	f.svc.Limiter(Binding{Database: f.db, HA: &fakeHA{state: HAState{Role: RolePrimary}},
		Budget:      &fakeBudget{state: BudgetState{Configured: true}},
		Concurrency: &fakeConcurrency{}}).AnnotateTrust(f.ctx, &v)
	AnnotateMode(&v, ceil)
	return v
}

func TestAutonomyModeDB_LedgerRowsCarryTheirMode(t *testing.T) {
	f := newFixture(t)
	f.seedLevel(FamilyTuning, ClassIndexCreate, L3)
	f.seedLevel(FamilyHygiene, ClassVacuum, L2)
	v := modeView(t, f, fullCeiling())
	idx, ok := trustRow(v, FamilyTuning, ClassIndexCreate)
	if !ok || idx.Effective == nil || *idx.Effective != L3 || idx.Mode != ModeActsInWindow {
		t.Fatalf("tuning/index_create = %+v, want L3 acts in the window", idx)
	}
	vac, _ := trustRow(v, FamilyHygiene, ClassVacuum)
	if vac.Mode != ModeAsksFirst || len(vac.ModeBecause) == 0 ||
		vac.ModeBecause[0] != "earned level L2" {
		t.Fatalf("hygiene/vacuum = %s %v, want asks first: earned level L2", vac.Mode,
			vac.ModeBecause)
	}
	for _, row := range v.Rows {
		if row.Mode == "" || len(row.ModeBecause) == 0 {
			t.Fatalf("row %s/%s has no mode: %+v", row.Family, row.Class, row)
		}
		if row.Reversibility == Irreversible &&
			(row.Mode == ModeActsThenTells || row.Mode == ModeActsInWindow) {
			t.Fatalf("irreversible %s/%s acts: %s", row.Family, row.Class, row.Mode)
		}
	}
}

func TestAutonomyModeDB_NoHASourceWatchesOnly(t *testing.T) {
	f := newFixture(t)
	f.seedLevel(FamilyTuning, ClassIndexCreate, L3)
	v, err := f.svc.TrustView(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.svc.Limiter(Binding{Database: f.db}).AnnotateTrust(f.ctx, &v)
	AnnotateMode(&v, fullCeiling())
	idx, _ := trustRow(v, FamilyTuning, ClassIndexCreate)
	if idx.Mode != ModeObserves || len(idx.ModeBecause) == 0 ||
		idx.ModeBecause[0] != "downgraded: ha_role_not_primary" {
		t.Fatalf("no HA source = %s %v, want observes: downgraded", idx.Mode, idx.ModeBecause)
	}
}

func TestAutonomyModeDB_LoweringTheCeilingDowngradesOnTheNextRead(t *testing.T) {
	f := newFixture(t)
	f.seedLevel(FamilyTuning, ClassIndexCreate, L3)
	before, _ := trustRow(modeView(t, f, fullCeiling()), FamilyTuning, ClassIndexCreate)
	lowered := modeCeiling(policy.TrustAdvisory, policy.ExecutionAuto, true, true)
	after, _ := trustRow(modeView(t, f, lowered), FamilyTuning, ClassIndexCreate)
	if before.Mode != ModeActsInWindow || after.Mode != ModeAsksFirst ||
		after.ModeBecause[0] != "trust ceiling advisory" {
		t.Fatalf("before %s, after %s %v; want acts in the window, then asks first",
			before.Mode, after.Mode, after.ModeBecause)
	}
}

func TestAutonomyModeDB_TwoAdminsApproveOnePromotion(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWraparound, 25, 0)
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		t.Fatalf("propose: %v", err)
	}
	p := f.pending(FamilyWraparound, ClassFreeze)
	if p == nil {
		t.Fatal("no pending wraparound_runway/freeze proposal")
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, admin := range []string{"user:1:a@example.com", "user:2:b@example.com"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.svc.Approve(f.ctx, p.ID, admin, "both clicked Approve")
		}()
	}
	wg.Wait()
	ok, refused := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrNotPending) || errors.Is(err, ErrConflict):
			refused++
		default:
			t.Fatalf("approve: unexpected error %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("approvals: %d succeeded, %d refused (%v); want one of each", ok, refused,
			errs)
	}
	if got := f.granted(FamilyWraparound, ClassFreeze); got != L2 {
		t.Fatalf("granted = %s, want L2 (one promotion)", got)
	}
	if left := f.pending(FamilyWraparound, ClassFreeze); left != nil {
		t.Fatalf("the proposal is still pending after approval: %+v", left)
	}
	row, _ := trustRow(modeView(t, f, fullCeiling()), FamilyWraparound, ClassFreeze)
	if row.Level != L2 || row.Pending != nil || row.Mode != ModeAsksFirst {
		t.Fatalf("re-read row = level %s pending %v mode %s; want L2, none, asks first",
			row.Level, row.Pending, row.Mode)
	}
}
