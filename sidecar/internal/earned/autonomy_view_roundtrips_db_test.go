package earned

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// earned-08 (2026-11-05): the Autonomy page polls GET
// /api/v1/sre/autonomy every 30 s. Its view read five evidence statements
// and a level for each of 25 pairs one after another, then the safety
// record once per family: 150+ sequential round trips on a host where
// each costs 10-350 ms. Evaluate did the same for 36 pairs. Both now read
// the ledger in one pipelined batch, the way the Trust view does.

// autonomyViewMaxRoundTrips bounds one annotated Autonomy view or one
// evaluation with nothing to propose: the proposal expiry, one batch of
// reads, and one spare.
const autonomyViewMaxRoundTrips = 3

// seedMixedLedger stores carried-over, grandfathered and earned levels
// and outcomes on many pairs, a harmful wraparound one included.
func seedMixedLedger(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := f.svc.SeedCarriedOver(f.ctx, f.db, autonomousBound()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SeedGrandfathered(f.ctx, f.db,
		rampBound(f.clock.Now(), 60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	recordSpread(t, f)
}

func annotatedAutonomyView(t *testing.T, svc *Service, lim *Limiter,
	rec *testdb.QueryRecorder) View {
	t.Helper()
	rec.Reset()
	v, err := svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lim.Annotate(context.Background(), &v)
	return v
}

func TestAutonomyViewTakesAtMostThreeRoundTrips(t *testing.T) {
	f := newReconFixture(t)
	svc, lim, rec := tracedLedger(t, f)
	v := annotatedAutonomyView(t, svc, lim, rec)
	if n := rec.RoundTrips(); n > autonomyViewMaxRoundTrips {
		t.Fatalf("an empty view took %d round trips, want <= %d:\n%s", n,
			autonomyViewMaxRoundTrips, statementList(rec))
	}
	if len(v.Families) != len(Families()) {
		t.Fatalf("empty view shows %d families, want %d", len(v.Families), len(Families()))
	}
	seedMixedLedger(t, f)
	v = annotatedAutonomyView(t, svc, lim, rec)
	if n := rec.RoundTrips(); n > autonomyViewMaxRoundTrips {
		t.Fatalf("a populated view took %d round trips, want <= %d:\n%s", n,
			autonomyViewMaxRoundTrips, statementList(rec))
	}
	provenances, rows := map[string]int{}, 0
	for _, fam := range v.Families {
		for _, row := range fam.Classes {
			rows++
			provenances[row.Provenance]++
			if row.Effective == nil {
				t.Fatalf("%s/%s has no effective level", fam.Family, row.Class)
			}
		}
	}
	// Grandfathering seeds only self-initiated classes (selfClasses), and
	// the Autonomy page lists only incident families, so no row there is
	// grandfathered; the Trust view shows those rows.
	if rows < 25 || provenances[ProvenanceCarriedOver] == 0 ||
		provenances[ProvenanceLedger] == 0 || provenances[ProvenanceGrandfathered] != 0 {
		t.Fatalf("view lost content: %d rows, provenances %v", rows, provenances)
	}
}

// The safety record is read once per annotated view, and each family keeps
// its own signal.
func TestAutonomyViewReadsTheSafetyRecordOnce(t *testing.T) {
	f := newReconFixture(t)
	svc, lim, rec := tracedLedger(t, f)
	recordSpread(t, f)
	v := annotatedAutonomyView(t, svc, lim, rec)
	if n := len(rec.Matching("result IN ('harmful', 'safety_violation')",
		"max(o.recorded_at)")); n != 1 {
		t.Fatalf("safety record read %d times, want once:\n%s", n, statementList(rec))
	}
	freeze, cancel := viewRow(v, FamilyWraparound, ClassFreeze),
		viewRow(v, FamilyLockBlocking, ClassBackendCancel)
	if freeze == nil || !hasDowngrade(freeze.Downgrades, DowngradeSafetyRegression) {
		t.Fatalf("wraparound lost its safety signal: %+v", freeze)
	}
	if cancel == nil || hasDowngrade(cancel.Downgrades, DowngradeSafetyRegression) {
		t.Fatalf("lock_blocking took wraparound's signal: %+v", cancel)
	}
}

func TestAutonomyViewEvaluateWithNothingToProposeTakesAtMostThree(t *testing.T) {
	f := newReconFixture(t)
	svc, _, rec := tracedLedger(t, f)
	rec.Reset()
	e, err := svc.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Created) != 0 || len(e.NotProposed) != applicablePairs() {
		t.Fatalf("fresh evaluation created %d, explained %d of %d pairs", len(e.Created),
			len(e.NotProposed), applicablePairs())
	}
	if n := rec.RoundTrips(); n > autonomyViewMaxRoundTrips {
		t.Fatalf("an evaluation with nothing to propose took %d round trips, want <= %d:\n%s",
			n, autonomyViewMaxRoundTrips, statementList(rec))
	}
}

// The Autonomy page shows the level the gate allows: a pair granted L1
// stays L1 whatever its evidence supports (the gate reads the evidence
// only at L2 and above).
func TestAutonomyViewShowsGrantedL1WhenTheEvidenceSupportsLess(t *testing.T) {
	lf := newLimiterFixture(t)
	v := View{Families: []FamilyView{{Family: FamilyWraparound, Classes: []ClassView{{
		Class: ClassFreeze, Cap: L3, Granted: L1, Supported: L0,
		Provenance: ProvenanceLedger}}}}}
	lf.lim.Annotate(lf.ctx, &v)
	if e := v.Families[0].Classes[0].Effective; e == nil || *e != L1 {
		t.Fatalf("granted L1 with supported L0 shows %v, the gate allows L1", e)
	}
	if got := lf.limit(freezeRequest(lf.clock.Now())); got.Level != 1 {
		t.Fatalf("gate allows %+v, want L1", got)
	}
}

// requestForPair is a gate request for the pair under a reversible
// contract with fresh evidence; ok is false for a pair no request maps to.
func requestForPair(f Family, c ActionClass, at time.Time) (policy.ActionRequest, bool) {
	spec, _ := Spec(c)
	req := policy.ActionRequest{TargetObjs: []string{"public.orders"},
		EvidenceObservedAt: at, Contract: &policy.ActionContract{
			RiskTier: policy.RiskSafe, RollbackClass: policy.RollbackReversible}}
	if len(spec.ActionTypes) > 0 {
		req.Contract.ActionType = spec.ActionTypes[0]
	}
	switch c {
	case ClassFreeze:
		req.Feature = string(policy.ChangeFreeze)
	case ClassWALBound:
		req.SQL = "ALTER SYSTEM SET max_slot_wal_keep_size = '96GB'"
	case ClassSlotDrop:
		req.SQL = "SELECT pg_drop_replication_slot('s')"
	}
	if !IsSelfInitiated(f) {
		req.IncidentFamily = string(f)
	}
	gf, gc := FamilyForRequest(req)
	return req, gf == f && gc == c
}

// On a mixed ledger every row of both pages shows the level Limit returns
// for a reversible request on that pair.
func TestAutonomyViewEffectiveLevelIsWhatTheGateAllows(t *testing.T) {
	lf := newLimiterFixture(t)
	lf.seedL3()
	seedMixedLedger(t, lf.fixture)
	v, err := lf.svc.View(lf.ctx)
	if err != nil {
		t.Fatal(err)
	}
	lf.lim.Annotate(lf.ctx, &v)
	tv, err := lf.svc.TrustView(lf.ctx)
	if err != nil {
		t.Fatal(err)
	}
	lf.lim.AnnotateTrust(lf.ctx, &tv)
	compared := 0
	check := func(page string, f Family, c ActionClass, effective *Level) {
		req, ok := requestForPair(f, c, lf.clock.Now())
		if !ok {
			return // no gate request maps to the pair
		}
		got := lf.limit(req)
		compared++
		if effective == nil || Level(got.Level) != *effective {
			t.Errorf("%s page %s/%s shows %v, the gate allows %+v", page, f, c, effective,
				got)
		}
	}
	for _, fam := range v.Families {
		for _, row := range fam.Classes {
			check("autonomy", fam.Family, row.Class, row.Effective)
		}
	}
	for _, row := range tv.Rows {
		check("trust", row.Family, row.Class, row.Effective)
	}
	if compared < 50 {
		t.Fatalf("compared %d rows with the gate, want both pages' rows (>= 50)", compared)
	}
}

func viewRow(v View, f Family, c ActionClass) *ClassView {
	for i := range v.Families {
		if v.Families[i].Family != f {
			continue
		}
		for j := range v.Families[i].Classes {
			if v.Families[i].Classes[j].Class == c {
				return &v.Families[i].Classes[j]
			}
		}
	}
	return nil
}
