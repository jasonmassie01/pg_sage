package earned

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Roadmap 1.2 migration: the autonomy the time ramp granted on the day
// the unified ledger took over is kept as explicitly labelled
// grandfathered levels, once per database. The ramp never grants again
// afterwards; grandfathered levels demote like any other.

func rampBound(now time.Time, rampAge time.Duration) policy.RuntimeState {
	return policy.RuntimeState{ExecutorEnabled: true, ExecutionMode: policy.ExecutionAuto,
		TrustLevel: policy.TrustAutonomous, Tier3Safe: true, Tier3Moderate: true,
		RampStart: now.Add(-rampAge)}
}

// selfClassesSeededAtL3 are the reversible self-initiated classes an
// elapsed autonomous ramp ran unattended (retention is irreversible).
var selfClassesSeededAtL3 = []ActionClass{ClassIndexCreate, ClassConfigGUC,
	ClassAutovacuumTuning, ClassQueryHint, ClassStatistics, ClassIndexDrop, ClassVacuum,
	ClassAnalyze, ClassReindex}

func (f *fixture) state(family Family, class ActionClass) State {
	f.t.Helper()
	st, err := f.svc.Granted(f.ctx, family, class)
	if err != nil {
		f.t.Fatalf("granted %s/%s: %v", family, class, err)
	}
	return st
}

func TestSeedGrandfatheredKeepsRampAutonomyOnce(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	rep, err := f.svc.SeedGrandfathered(f.ctx, f.db, rampBound(now, 60*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Migrated || rep.Database != f.db || !rep.MigratedAt.Equal(now) ||
		len(rep.Seeded) != len(selfClassesSeededAtL3) {
		t.Fatalf("report = %+v", rep)
	}
	for _, c := range selfClassesSeededAtL3 {
		st := f.state(SelfFamilyFor(c), c)
		if st.Level != L3 || st.Provenance != ProvenanceGrandfathered || !st.Stored ||
			!strings.Contains(st.CarriedRef, "autonomous") || st.ChangedBy != ActorPgSage {
			t.Errorf("%s: %+v", c, st)
		}
	}
	if st := f.state(FamilyHygiene, ClassRetention); st.Level != L1 || st.Stored {
		t.Fatalf("retention (irreversible) was grandfathered: %+v", st)
	}
	evs := f.events(EventFilter{Family: FamilyTuning, Class: ClassIndexCreate,
		Database: f.db})
	if len(evs) != 1 || evs[0].Type != EventGrandfathered || evs[0].To == nil ||
		*evs[0].To != L3 || evs[0].From == nil || *evs[0].From != L1 {
		t.Fatalf("grandfather events = %+v", evs)
	}
	// A second start, even with a stricter configuration, changes nothing.
	again, err := f.svc.SeedGrandfathered(f.ctx, f.db, policy.RuntimeState{})
	if err != nil || again.Migrated || len(again.Seeded) != 0 ||
		!again.MigratedAt.Equal(now) {
		t.Fatalf("second seeding = %+v (%v)", again, err)
	}
	if st := f.state(FamilyTuning, ClassIndexCreate); st.Level != L3 {
		t.Fatalf("a second seeding changed the level: %+v", st)
	}
}

// The ramp elapsing after the migration grants nothing: the marker is
// written even when nothing was seeded.
func TestSeedGrandfatheredRampNeverGrantsLater(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	young := rampBound(now, time.Hour)
	rep, err := f.svc.SeedGrandfathered(f.ctx, f.db, young)
	if err != nil || !rep.Migrated || len(rep.Seeded) != 0 {
		t.Fatalf("young ramp seeding = %+v (%v)", rep, err)
	}
	f.clock.Advance(60 * 24 * time.Hour)
	rep, err = f.svc.SeedGrandfathered(f.ctx, f.db, young)
	if err != nil || rep.Migrated || len(rep.Seeded) != 0 {
		t.Fatalf("elapsed ramp seeded after the migration: %+v (%v)", rep, err)
	}
	if st := f.state(FamilyHygiene, ClassVacuum); st.Level != L1 {
		t.Fatalf("vacuum = %v after the ramp elapsed, want L1", st.Level)
	}
	got, err := f.svc.Grandfathered(f.ctx)
	if err != nil || got == nil || !got.MigratedAt.Equal(now) {
		t.Fatalf("grandfather record = %+v (%v)", got, err)
	}
}

func TestSeedGrandfatheredAdvisoryKeepsApprovals(t *testing.T) {
	f := newFixture(t)
	bound := rampBound(f.clock.Now(), time.Hour)
	bound.TrustLevel = policy.TrustAdvisory
	if _, err := f.svc.SeedGrandfathered(f.ctx, f.db, bound); err != nil {
		t.Fatal(err)
	}
	if st := f.state(FamilyTuning, ClassIndexCreate); st.Level != L2 ||
		st.Provenance != ProvenanceGrandfathered {
		t.Fatalf("advisory index_create = %+v, want grandfathered L2", st)
	}
	if st := f.state(FamilyHygiene, ClassVacuum); st.Level != L1 {
		t.Fatalf("advisory vacuum before the safe ramp = %v, want L1", st.Level)
	}
}

// An existing ledger row is never raised or lowered by the migration.
func TestSeedGrandfatheredKeepsExistingRows(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.Downgrade(f.ctx, DowngradeRequest{Family: FamilyHygiene,
		Class: ClassIndexDrop, To: L0, Actor: "user:1:a@b", Reason: "drops off"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SeedGrandfathered(f.ctx, f.db,
		rampBound(f.clock.Now(), 90*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if st := f.state(FamilyHygiene, ClassIndexDrop); st.Level != L0 ||
		st.Provenance != ProvenanceLedger {
		t.Fatalf("an operator's restriction was overwritten: %+v", st)
	}
}

func TestSeedGrandfatheredIsPerDatabase(t *testing.T) {
	f := newFixture(t)
	g := f.sibling("billing")
	now := f.clock.Now()
	bound := rampBound(now, 60*24*time.Hour)
	if _, err := f.svc.SeedGrandfathered(f.ctx, f.db, bound); err != nil {
		t.Fatal(err)
	}
	if st := g.state(FamilyHygiene, ClassVacuum); st.Level != L1 {
		t.Fatalf("billing inherited orders' grandfathered level: %+v", st)
	}
	rep, err := g.svc.SeedGrandfathered(g.ctx, g.db, policy.RuntimeState{})
	if err != nil || !rep.Migrated || len(rep.Seeded) != 0 {
		t.Fatalf("billing seeding = %+v (%v)", rep, err)
	}
	if _, err := f.svc.SeedGrandfathered(f.ctx, "billing", policy.RuntimeState{}); !errors.Is(
		err, ErrInvalidRequest) {
		t.Fatalf("seeding another database's ledger: %v", err)
	}
}

func TestSeedGrandfatheredConcurrentStartsSeedOnce(t *testing.T) {
	f := newFixture(t)
	bound := rampBound(f.clock.Now(), 60*24*time.Hour)
	var wg sync.WaitGroup
	var mu sync.Mutex
	migrated, seeded := 0, 0
	var errs []error
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, err := f.svc.SeedGrandfathered(context.Background(), f.db, bound)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if rep.Migrated {
				migrated++
			}
			seeded += len(rep.Seeded)
		}()
	}
	wg.Wait()
	if len(errs) != 0 || migrated != 1 || seeded != len(selfClassesSeededAtL3) {
		t.Fatalf("migrated %d, seeded %d, errors %v", migrated, seeded, errs)
	}
	evs := f.events(EventFilter{Family: FamilyHygiene, Class: ClassVacuum, Database: f.db})
	if len(evs) != 1 {
		t.Fatalf("vacuum grandfather events = %d, want 1", len(evs))
	}
}

// A grandfathered level demotes like any other: an operator downgrade
// applies, and the row stops being labelled grandfathered.
func TestGrandfatheredLevelDemotesNormally(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.SeedGrandfathered(f.ctx, f.db,
		rampBound(f.clock.Now(), 60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute)
	out, err := f.svc.Downgrade(f.ctx, DowngradeRequest{Family: FamilyTuning,
		Class: ClassConfigGUC, To: L2, Actor: "user:1:a@b", Reason: "GUCs by hand for now"})
	if err != nil || len(out) != 1 || out[0].Level != L2 ||
		out[0].Provenance != ProvenanceLedger || out[0].CarriedRef != "" {
		t.Fatalf("downgrade of a grandfathered level = %+v (%v)", out, err)
	}
}

// Shadow reviews are investigation evidence: a self-initiated family
// takes none.
func TestRecordReviewRefusesSelfInitiatedFamilies(t *testing.T) {
	f := newFixture(t)
	err := f.svc.RecordReview(f.ctx, Review{Database: f.db, InvestigationID: newUUID(t),
		Family: FamilyTuning, Verdict: VerdictAccepted, Reviewer: "user:1:a@b"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("review of a tuning packet: %v", err)
	}
}
