package earned

import (
	"encoding/json"
	"testing"
	"time"
)

// The Trust page reads one view per database: every family x class of
// both kinds with its level, evidence counts (improved, neutral,
// regressed, rolled back, rejected), the last change and why, and the
// path to the next level.

func trustRow(v TrustView, f Family, c ActionClass) (TrustRow, bool) {
	for _, r := range v.Rows {
		if r.Family == f && r.Class == c {
			return r, true
		}
	}
	return TrustRow{}, false
}

func TestTrustViewCoversBothKinds(t *testing.T) {
	f := newSelfFixture(t)
	f.svc.WithRamp(func() RampFloor {
		return RampFloor{Start: f.clock.Now().Add(-2 * time.Hour), Safe: time.Hour,
			Moderate: 3 * time.Hour}
	})
	f.grandfatherAt(L3)
	f.selfAction("create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i1 ON public.orders (a)", "index_create", "success",
		"improved", "autonomy_l3", time.Minute)
	f.selfAction("create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i2 ON public.orders (b)", "index_create", "success",
		"neutral", "autonomy_l3", time.Minute)
	f.rejectedProposal("create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i3 ON public.orders (c)", "")
	if _, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	v, err := f.svc.TrustView(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v.Database != f.db || v.Grandfathered == nil || v.Floor == nil || !v.Floor.Known {
		t.Fatalf("view header = %+v", v)
	}
	row, ok := trustRow(v, FamilyTuning, ClassIndexCreate)
	if !ok || row.Kind != KindSelfInitiated || row.OutcomeClass != "index_create" ||
		row.Level != L2 || row.Cap != L3 {
		t.Fatalf("tuning/index_create = %+v", row)
	}
	want := TrustCounts{Improved: 1, Neutral: 1, Rejected: 1}
	if row.Evidence != want {
		t.Fatalf("evidence = %+v, want %+v", row.Evidence, want)
	}
	if row.LastChange.Event != EventAutoDowngraded || row.LastChange.At == nil ||
		row.LastChange.Reason == "" {
		t.Fatalf("last change = %+v", row.LastChange)
	}
	if row.Next == nil || row.Next.Target != L3 {
		t.Fatalf("next = %+v", row.Next)
	}
	vac, _ := trustRow(v, FamilyHygiene, ClassVacuum)
	if vac.Provenance != ProvenanceGrandfathered || vac.ProvenanceRef == "" ||
		vac.LastChange.Event != EventGrandfathered || vac.Next != nil {
		t.Fatalf("hygiene/vacuum = %+v", vac)
	}
	freeze, ok := trustRow(v, FamilyWraparound, ClassFreeze)
	if !ok || freeze.Kind != KindIncident || freeze.Level != L1 || freeze.OutcomeClass != "" {
		t.Fatalf("wraparound/freeze = %+v", freeze)
	}
	ret, _ := trustRow(v, FamilyHygiene, ClassRetention)
	if ret.Cap != L1 || ret.Reversibility != Irreversible || ret.Next != nil {
		t.Fatalf("retention = %+v", ret)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(raw, &round); err != nil || round["rows"] == nil ||
		round["database"] != f.db {
		t.Fatalf("json = %s", raw)
	}
}

// The limiter's annotation adds the effective level and the active
// downgrade signals for the database.
func TestTrustViewAnnotateEffective(t *testing.T) {
	lf := newLimiterFixture(t)
	if _, err := lf.svc.SeedGrandfathered(lf.ctx, lf.db,
		autonomousBound(lf.clock.Now(), 60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	v, err := lf.svc.TrustView(lf.ctx)
	if err != nil {
		t.Fatal(err)
	}
	lf.lim.AnnotateTrust(lf.ctx, &v)
	row, _ := trustRow(v, FamilyHygiene, ClassVacuum)
	if row.Effective == nil || *row.Effective != L3 || len(row.Downgrades) != 0 {
		t.Fatalf("effective = %+v", row)
	}
	lf.budget.state = BudgetState{Configured: true, FastBurning: true}
	v, _ = lf.svc.TrustView(lf.ctx)
	lf.lim.AnnotateTrust(lf.ctx, &v)
	row, _ = trustRow(v, FamilyHygiene, ClassVacuum)
	if row.Effective == nil || *row.Effective != L1 || len(row.Downgrades) == 0 {
		t.Fatalf("effective under a budget burn = %+v", row)
	}
}

func TestTrustViewWithoutMigration(t *testing.T) {
	f := newFixture(t)
	v, err := f.svc.TrustView(f.ctx)
	if err != nil || v.Grandfathered != nil || v.Floor == nil || v.Floor.Known {
		t.Fatalf("fresh view = %+v (%v)", v, err)
	}
	row, ok := trustRow(v, FamilyHygiene, ClassAnalyze)
	if !ok || row.Level != L1 || row.LastChange.At != nil || row.Next == nil {
		t.Fatalf("fresh hygiene/analyze = %+v", row)
	}
}
