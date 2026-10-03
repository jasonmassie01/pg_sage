package earned

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Carry-over (coordinator decision 2026-10-02): M7 gates NEW autonomy. A
// pair that pg_sage already ran autonomously under decided policy before
// the ledger existed (custodian freeze and autovacuum tuning, spec F3;
// the WAL bound, F4; load-admitted index creation, D6) is seeded at the
// level today's configuration allows, with provenance carried_over and
// the decision that granted it. Never above what the configuration
// allows, never above L1 for an irreversible class, never L4. Everything
// else starts at L1. A carried pair is still capped by every downgrade
// signal and comes back by itself when the signal clears: it was granted
// by policy, not earned by evidence, so it does not decay with evidence
// and a safety regression caps it for the safety window instead of
// demoting it durably.

func autonomousBound() policy.RuntimeState {
	return policy.RuntimeState{ExecutorEnabled: true, ExecutionMode: policy.ExecutionAuto,
		TrustLevel: policy.TrustAutonomous, Tier3Safe: true, Tier3Moderate: true}
}

var carriedPairs = []pairKey{{FamilyWraparound, ClassFreeze},
	{FamilyWraparound, ClassAutovacuumTuning}, {FamilyWAL, ClassWALBound},
	{FamilyPlanRegression, ClassIndexCreate}}

func TestCarriedLevelFollowsTheOperatorBound(t *testing.T) {
	advisory := autonomousBound()
	advisory.TrustLevel = policy.TrustAdvisory
	noSafe := autonomousBound()
	noSafe.Tier3Safe = false
	noModerate := autonomousBound()
	noModerate.Tier3Moderate = false
	cases := map[string]struct {
		bound          policy.RuntimeState
		freeze, walBnd Level
	}{
		"autonomous":         {autonomousBound(), L3, L3},
		"advisory":           {advisory, L3, L1}, // safe only under advisory trust
		"tier3 safe off":     {noSafe, L1, L3},
		"tier3 moderate off": {noModerate, L3, L1},
		"approval mode": {func() policy.RuntimeState {
			b := autonomousBound()
			b.ExecutionMode = policy.ExecutionApproval
			return b
		}(), L1, L1},
		"manual mode": {func() policy.RuntimeState {
			b := autonomousBound()
			b.ExecutionMode = policy.ExecutionManual
			return b
		}(), L1, L1},
		"observation": {func() policy.RuntimeState {
			b := autonomousBound()
			b.TrustLevel = policy.TrustObservation
			return b
		}(), L1, L1},
		"executor disabled": {func() policy.RuntimeState {
			b := autonomousBound()
			b.ExecutorEnabled = false
			return b
		}(), L1, L1},
		"empty": {policy.RuntimeState{}, L1, L1},
	}
	for name, c := range cases {
		freeze := CarriedLevel(c.bound, FamilyWraparound, ClassFreeze)
		wal := CarriedLevel(c.bound, FamilyWAL, ClassWALBound)
		if freeze != c.freeze || wal != c.walBnd {
			t.Errorf("%s: freeze %v wal_bound %v, want %v %v", name, freeze, wal, c.freeze,
				c.walBnd)
		}
	}
}

func TestCarryOverNeverExceedsL1ForIrreversibleOrReachesL4(t *testing.T) {
	for _, f := range Families() {
		for _, spec := range Classes() {
			got := CarriedLevel(autonomousBound(), f, spec.Class)
			if got > L3 || (spec.Reversibility == Irreversible && got > L1) {
				t.Errorf("%s/%s carried at %v", f, spec.Class, got)
			}
			if got > L1 && !carried(f, spec.Class) {
				t.Errorf("%s/%s was not autonomous before M7 but carries %v", f, spec.Class,
					got)
			}
		}
	}
	for _, co := range CarryOvers() {
		if co.Ref == "" {
			t.Errorf("%s/%s carries no decision reference", co.Family, co.Class)
		}
	}
}

func carried(f Family, c ActionClass) bool {
	for _, p := range carriedPairs {
		if p.family == f && p.class == c {
			return true
		}
	}
	return false
}

func TestSeedCarriesOverTodaysAutonomy(t *testing.T) {
	f := newFixture(t)
	seeded, err := f.svc.SeedCarriedOver(f.ctx, "orders", autonomousBound())
	if err != nil || len(seeded) != len(carriedPairs) {
		t.Fatalf("seeded = %+v (%v)", seeded, err)
	}
	refs := map[pairKey]string{}
	for _, st := range seeded {
		refs[pairKey{st.Family, st.Class}] = st.CarriedRef
		if st.Level != L3 || st.Provenance != ProvenanceCarriedOver ||
			st.ChangedBy != ActorPgSage {
			t.Errorf("seeded %s/%s = %+v", st.Family, st.Class, st)
		}
	}
	for ref, want := range map[pairKey]string{
		{FamilyWraparound, ClassFreeze}: "F3", {FamilyWAL, ClassWALBound}: "F4",
		{FamilyPlanRegression, ClassIndexCreate}: "D6",
	} {
		if !strings.Contains(refs[ref], want) {
			t.Errorf("%s/%s ref = %q, want it to cite %s", ref.family, ref.class, refs[ref],
				want)
		}
	}
	if f.granted(FamilyWraparound, ClassFreeze) != L3 ||
		f.granted(FamilyLockBlocking, ClassBackendCancel) != L1 ||
		f.granted(FamilyWraparound, ClassBackendTerminate) != L1 {
		t.Fatal("seeding changed a pair that was not autonomous before")
	}
	evs := f.events(EventFilter{Family: FamilyWAL, Class: ClassWALBound})
	if len(evs) != 1 || evs[0].Type != EventCarriedOver || evs[0].To == nil ||
		*evs[0].To != L3 || evs[0].Database != "orders" || evs[0].Reason == "" {
		t.Fatalf("carry-over event = %+v", evs)
	}
}

func TestSeedIsIdempotentAndNeverRaisesAnExistingRow(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Downgrade(f.ctx, DowngradeRequest{Family: FamilyWAL,
		Class: ClassWALBound, To: L0, Actor: "user:2:ops@example.com",
		Reason: "replica lag drill"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		seeded, err := f.svc.SeedCarriedOver(f.ctx, "orders", autonomousBound())
		want := len(carriedPairs) - 1
		if i == 1 {
			want = 0
		}
		if err != nil || len(seeded) != want {
			t.Fatalf("seed %d = %d states (%v), want %d", i, len(seeded), err, want)
		}
	}
	if st, _ := f.svc.Granted(f.ctx, FamilyWAL, ClassWALBound); st.Level != L0 ||
		st.Provenance == ProvenanceCarriedOver {
		t.Fatalf("seeding overrode the operator's downgrade: %+v", st)
	}
	if n := len(f.events(EventFilter{Family: FamilyWraparound, Class: ClassFreeze})); n != 1 {
		t.Fatalf("freeze carry-over events = %d, want 1", n)
	}
	// Carry-over is per database (P0-5): a database whose configuration
	// allows less neither lowers what another database carried nor
	// inherits it.
	billing := f.sibling("billing")
	if seeded, err := billing.svc.SeedCarriedOver(billing.ctx, "billing",
		policy.RuntimeState{}); err != nil || len(seeded) != 0 {
		t.Fatalf("strict billing seeded %+v (%v)", seeded, err)
	}
	if f.granted(FamilyWraparound, ClassFreeze) != L3 {
		t.Fatal("a stricter database lowered the carried level")
	}
	if billing.granted(FamilyWraparound, ClassFreeze) != L1 {
		t.Fatal("billing inherited the carry-over of orders")
	}
}

func TestSeedWithoutAutonomyCarriesNothing(t *testing.T) {
	f := newFixture(t)
	bound := autonomousBound()
	bound.ExecutionMode = policy.ExecutionApproval
	seeded, err := f.svc.SeedCarriedOver(f.ctx, "orders", bound)
	if err != nil || len(seeded) != 0 {
		t.Fatalf("seeded under approval mode = %+v (%v)", seeded, err)
	}
	if f.granted(FamilyWraparound, ClassFreeze) != L1 {
		t.Fatal("a pair that was not autonomous was carried")
	}
}

func walBoundRequest(at time.Time) policy.ActionRequest {
	return policy.ActionRequest{IncidentFamily: string(FamilyWAL), Feature: "config_guc",
		SQL:        "ALTER SYSTEM SET max_slot_wal_keep_size = '96GB'",
		TargetObjs: []string{"max_slot_wal_keep_size"}, EvidenceObservedAt: at,
		Contract: &policy.ActionContract{ActionType: "alter_system_guc",
			RiskTier: policy.RiskModerate, RollbackClass: policy.RollbackReversible}}
}

// A carried pair keeps its level without promotion evidence (it was
// granted by policy), above the product cap of earned promotion.
func TestCarriedLevelHoldsWithoutEvidence(t *testing.T) {
	lf := newLimiterFixture(t)
	if _, err := lf.svc.SeedCarriedOver(lf.ctx, "orders", autonomousBound()); err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]policy.ActionRequest{
		"freeze": freezeRequest(lf.clock.Now()), "wal_bound": walBoundRequest(lf.clock.Now()),
	} {
		if got := lf.limit(req); got.Level != 3 || got.Downgraded || got.Granted != 3 {
			t.Errorf("%s carried limit = %+v, want L3", name, got)
		}
	}
	if _, err := lf.svc.ProposePromotions(lf.ctx); err != nil {
		t.Fatal(err)
	}
	if p := lf.pending(FamilyWAL, ClassWALBound); p != nil {
		t.Fatalf("a carried pair was proposed for promotion: %+v", p)
	}
}

// Downgrade signals cap a carried pair, and it returns to its carried
// level by itself when they clear, each transition recorded.
func TestCarriedPairIsCappedAndRestoredAutomatically(t *testing.T) {
	lf := newLimiterFixture(t)
	if _, err := lf.svc.SeedCarriedOver(lf.ctx, "orders", autonomousBound()); err != nil {
		t.Fatal(err)
	}
	lf.budget.state.FastBurning = true
	if got := lf.limit(freezeRequest(lf.clock.Now())); got.Level != 1 || !got.Downgraded {
		t.Fatalf("burning = %+v", got)
	}
	lf.budget.state.FastBurning = false
	lf.ha.state = HAState{Role: RoleReplica}
	if got := lf.limit(freezeRequest(lf.clock.Now())); got.Level != 1 || !got.Downgraded {
		t.Fatalf("failover = %+v", got)
	}
	lf.ha.state = HAState{Role: RolePrimary}
	if got := lf.limit(freezeRequest(lf.clock.Now())); got.Level != 3 || got.Downgraded {
		t.Fatalf("after the signals cleared = %+v, want L3 again", got)
	}
	evs := lf.events(EventFilter{Family: FamilyWraparound, Class: ClassFreeze,
		Database: "orders"})
	var types []EventType
	for _, e := range evs {
		if e.Type != EventCarriedOver {
			types = append(types, e.Type)
		}
	}
	want := []EventType{EventCapCleared, EventCapped, EventCapped} // newest first
	if len(types) != len(want) || types[0] != want[0] || types[1] != want[1] ||
		types[2] != want[2] {
		t.Fatalf("transitions = %v, want %v", types, want)
	}
	if st, _ := lf.svc.Granted(lf.ctx, FamilyWraparound, ClassFreeze); st.Level != L3 ||
		st.Provenance != ProvenanceCarriedOver {
		t.Fatalf("a transient downgrade changed the carried grant: %+v", st)
	}
}

// A safety regression caps a carried pair for the safety window instead
// of demoting it durably; earned pairs of the family are still demoted.
func TestSafetyRegressionCapsACarriedPairForTheWindow(t *testing.T) {
	lf := newLimiterFixture(t)
	if _, err := lf.svc.SeedCarriedOver(lf.ctx, "orders", autonomousBound()); err != nil {
		t.Fatal(err)
	}
	if err := lf.svc.RecordOutcome(lf.ctx, Outcome{Database: "orders",
		Family: FamilyWraparound, Class: ClassFreeze, Level: L3, Result: ResultHarmful,
		Source: SourceOperator, Actor: "user:2:ops@example.com"}); err != nil {
		t.Fatal(err)
	}
	st, _ := lf.svc.Granted(lf.ctx, FamilyWraparound, ClassFreeze)
	if st.Level != L3 || st.Provenance != ProvenanceCarriedOver {
		t.Fatalf("carried pair after a harmful outcome = %+v", st)
	}
	got := lf.limit(freezeRequest(lf.clock.Now()))
	if got.Level != 1 || !got.Downgraded {
		t.Fatalf("inside the safety window = %+v, want capped at L1", got)
	}
	lf.clock.Advance(31 * 24 * time.Hour)
	if got := lf.limit(freezeRequest(lf.clock.Now())); got.Level != 3 || got.Downgraded {
		t.Fatalf("after the safety window = %+v, want L3", got)
	}
}

// An operator's downgrade ends the carry-over: the pair then has to earn
// its level back with evidence.
func TestOperatorDowngradeEndsTheCarryOver(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.SeedCarriedOver(f.ctx, "orders", autonomousBound()); err != nil {
		t.Fatal(err)
	}
	sts, err := f.svc.Downgrade(f.ctx, DowngradeRequest{Family: FamilyWraparound,
		Class: ClassFreeze, To: L1, Actor: "user:2:ops@example.com", Reason: "audit"})
	if err != nil || len(sts) != 1 || sts[0].Provenance == ProvenanceCarriedOver {
		t.Fatalf("downgrade = %+v (%v)", sts, err)
	}
	if _, err := f.svc.SeedCarriedOver(f.ctx, "orders", autonomousBound()); err != nil {
		t.Fatal(err)
	}
	if f.granted(FamilyWraparound, ClassFreeze) != L1 {
		t.Fatal("re-seeding restored a pair the operator downgraded")
	}
}

func TestViewShowsTheCarryOver(t *testing.T) {
	f := newLimiterFixture(t)
	if _, err := f.svc.SeedCarriedOver(f.ctx, "orders", autonomousBound()); err != nil {
		t.Fatal(err)
	}
	v, err := f.svc.View(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.lim.Annotate(f.ctx, &v)
	var row, plain *ClassView
	for i := range v.Families {
		for j := range v.Families[i].Classes {
			c := &v.Families[i].Classes[j]
			switch {
			case v.Families[i].Family == FamilyWAL && c.Class == ClassWALBound:
				row = c
			case v.Families[i].Family == FamilyLockBlocking && c.Class == ClassBackendCancel:
				plain = c
			}
		}
	}
	if row == nil || row.Provenance != ProvenanceCarriedOver ||
		!strings.Contains(row.CarriedRef, "F4") || row.Granted != L3 ||
		row.Effective == nil || *row.Effective != L3 {
		t.Fatalf("carried row = %+v", row)
	}
	if plain == nil || plain.Provenance == ProvenanceCarriedOver {
		t.Fatalf("plain row = %+v", plain)
	}
}

// A mandatory deadline override is not restricted by the ledger, which
// records it once per pair, target and deadline.
func TestDeadlineOverrideIsRecordedOnce(t *testing.T) {
	lf := newLimiterFixture(t)
	req := freezeRequest(lf.clock.Now())
	req.Deadline = &policy.DeadlineContext{Kind: policy.DeadlineXID,
		Urgency: policy.UrgencyCritical, HardAt: lf.clock.Now().Add(6 * time.Hour)}
	d := policy.Decision{Verdict: policy.VerdictExecute, Reason: policy.ReasonDeadlineOverride}
	for i := 0; i < 3; i++ {
		lf.lim.RecordDeadlineOverride(lf.ctx, req, d)
	}
	evs := lf.events(EventFilter{Family: FamilyWraparound, Class: ClassFreeze,
		Database: "orders"})
	if len(evs) != 1 || evs[0].Type != EventDeadlineOverride ||
		!strings.Contains(evs[0].Reason, "xid") || evs[0].Actor != ActorPgSage {
		t.Fatalf("deadline override events = %+v", evs)
	}
	other := req
	other.TargetObjs = []string{"public.items"}
	lf.lim.RecordDeadlineOverride(lf.ctx, other, d)
	if n := len(lf.events(EventFilter{Family: FamilyWraparound, Class: ClassFreeze,
		Database: "orders"})); n != 2 {
		t.Fatalf("a second table's deadline was not recorded: %d events", n)
	}
}
