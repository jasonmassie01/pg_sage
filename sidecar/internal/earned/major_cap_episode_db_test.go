package earned

import (
	"strings"
	"testing"
	"time"
)

// earned-07 review: which outcomes re-validate a major cap, and a second
// upgrade after a cleared one.

// outcomes records n vacuum outcomes with verdict, a minute apart, from id.
func (mf *majorFixture) outcomes(n int, id int64, verdict, result string) {
	mf.t.Helper()
	for i := 0; i < n; i++ {
		mf.clock.Advance(time.Minute)
		if err := mf.svc.RecordOutcome(mf.ctx, Outcome{Database: mf.db,
			ActionLogID: id + int64(i), Family: FamilyHygiene, Class: ClassVacuum, Level: L2,
			Result: result, Verdict: verdict, Source: SourceExecutor,
			Actor: ActorPgSage}); err != nil {
			mf.t.Fatalf("record %s outcome: %v", verdict, err)
		}
	}
}

// Only improved outcomes count: neutral and insufficient ones on the new
// major do not re-validate the level.
func TestMajorCapNeedsImprovedOutcomes(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedSelfL3()
	mf.major.set(17, nil)
	mf.limit(selfVacuum("public.orders"))
	mf.outcomes(3, 8100, "neutral", ResultUnverified)
	mf.outcomes(3, 8200, "insufficient_evidence", ResultUnverified)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 2 {
		t.Fatalf("after neutral and insufficient outcomes = %+v, want L2", got)
	}
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); !intIs(got, 16) {
		t.Fatalf("server_major = %v, want 16 kept", got)
	}
	mf.improved(2, 8300)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 2 {
		t.Fatalf("after 2 improved = %+v, want L2 (threshold is 3)", got)
	}
	mf.improved(1, 8400)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 3 {
		t.Fatalf("after 3 improved = %+v, want L3", got)
	}
}

// A second upgrade after a re-validated one needs its own improved
// outcomes: those of the first episode do not carry over.
func TestMajorCapSecondUpgradeStartsAFreshCount(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedSelfL3()
	mf.major.set(17, nil)
	mf.limit(selfVacuum("public.orders"))
	mf.improved(3, 8500)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 3 {
		t.Fatalf("after 3 improved on 17 = %+v, want L3", got)
	}
	mf.improved(2, 8600) // on 17, before the next upgrade
	mf.major.set(18, nil)
	mf.clock.Advance(time.Minute)
	got := mf.limit(selfVacuum("public.orders"))
	if got.Level != 2 ||
		!strings.Contains(strings.Join(got.Reasons, ","), "server_major_changed:17->18") {
		t.Fatalf("after 17->18 = %+v, want L2 capped 17->18", got)
	}
	mf.improved(2, 8700)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 2 {
		t.Fatalf("2 improved on 18 = %+v, want L2", got)
	}
	mf.improved(1, 8800)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 3 {
		t.Fatalf("3 improved on 18 = %+v, want L3", got)
	}
	evs := mf.capEvents(FamilyHygiene, ClassVacuum)
	var types []string
	for _, e := range evs {
		types = append(types, string(e.Type))
	}
	want := "capped,cap_cleared,capped,cap_cleared"
	if strings.Join(types, ",") != want {
		t.Fatalf("cap events = %v, want %s", types, want)
	}
	if r := evs[3].Reason; r != "PostgreSQL 17→18 re-validated by 3 improved outcomes "+
		"on PostgreSQL 18" {
		t.Fatalf("clearing reason = %q, want it to name the improved outcomes", r)
	}
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); !intIs(got, 18) {
		t.Fatalf("server_major = %v, want 18", got)
	}
}

// A demotion below L3 ends the major cap: the level is no longer one that
// re-validates, and a later promotion on the new major is not capped.
func TestMajorCapEndsWhenTheLevelDropsBelowL3(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedSelfL3()
	mf.major.set(17, nil)
	mf.limit(selfVacuum("public.orders"))
	if _, err := mf.svc.Downgrade(mf.ctx, DowngradeRequest{Family: FamilyHygiene,
		Class: ClassVacuum, To: L2, Actor: "user:2:ops@example.com",
		Reason: "after the upgrade"}); err != nil {
		t.Fatal(err)
	}
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 2 || len(got.Reasons) != 0 {
		t.Fatalf("L2 after the demotion = %+v", got)
	}
	evs := mf.capEvents(FamilyHygiene, ClassVacuum)
	if len(evs) != 2 || evs[1].Type != EventCapCleared ||
		!strings.Contains(evs[1].Reason, "below L3") {
		t.Fatalf("cap events = %+v, want capped then cleared below L3", evs)
	}
}

// A server put back on the granted major ends the cap; when it is upgraded
// again, only improved outcomes after the new cap count, not those of the
// earlier episode or of the time back on the granted major.
func TestMajorCapRollbackEndsTheEpisode(t *testing.T) {
	mf := newMajorFixture(t, 16)
	mf.seedSelfL3()
	mf.major.set(17, nil)
	mf.limit(selfVacuum("public.orders"))
	mf.improved(2, 8900)
	mf.major.set(16, nil) // the upgrade was rolled back
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 3 {
		t.Fatalf("back on 16 = %+v, want L3", got)
	}
	evs := mf.capEvents(FamilyHygiene, ClassVacuum)
	if len(evs) != 2 || evs[1].Type != EventCapCleared ||
		!strings.Contains(evs[1].Reason, "runs PostgreSQL 16 again") {
		t.Fatalf("cap events = %+v, want the cap ended by the rollback", evs)
	}
	mf.improved(3, 9000) // on 16
	mf.major.set(17, nil)
	mf.clock.Advance(time.Minute)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 2 {
		t.Fatalf("upgraded again = %+v, want L2: earlier outcomes do not count", got)
	}
	if got := mf.storedMajor(FamilyHygiene, ClassVacuum); !intIs(got, 16) {
		t.Fatalf("server_major = %v, want 16 until re-validated", got)
	}
	mf.improved(3, 9100)
	if got := mf.limit(selfVacuum("public.orders")); got.Level != 3 {
		t.Fatalf("3 improved after the second cap = %+v, want L3", got)
	}
}
