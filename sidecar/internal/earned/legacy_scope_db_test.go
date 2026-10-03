package earned

import (
	"context"
	"errors"
	"testing"
)

// P0-5 migration decision: a level stored before the ledger was per
// database (database_name '') whose database cannot be determined is a
// deployment-wide legacy level. Each database of the deployment adopts it
// once, at its current level, when it binds (AdoptLegacy), unless the
// database already has its own row; the adoption is recorded. Earned
// levels stay capped by the adopting database's own evidence, so an
// adopted promotion grants nothing that database has not earned, while an
// adopted restriction (a downgrade) keeps holding. A legacy carried-over
// level is not adopted: carry-over is seeded per database from that
// database's own configuration (SeedCarriedOver).

// legacyRow stores a pre-scoping ledger row for the fixture's deployment.
func (f *fixture) legacyRow(family Family, class ActionClass, level Level,
	provenance string) {
	f.t.Helper()
	var ref any
	if provenance == ProvenanceCarriedOver {
		ref = "spec F3"
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO sage.sre_family_autonomy
		(deployment_id, database_name, family, action_class, level, changed_by,
		 change_reason, provenance, carried_ref)
		VALUES ($1, '', $2, $3, $4, 'user:1:admin@example.com', 'before database scope',
		        $5, $6)`, f.store.DeploymentID(), string(family), string(class), int16(level),
		provenance, ref); err != nil {
		f.t.Fatalf("legacy row: %v", err)
	}
}

func TestAdoptLegacyCopiesDeploymentLevelsToEachDatabaseOnce(t *testing.T) {
	orders, billing := fleetPair(t)
	orders.legacyRow(FamilyWAL, ClassWALBound, L2, ProvenanceLedger)
	orders.legacyRow(FamilyLockBlocking, ClassBackendCancel, L0, ProvenanceLedger)
	orders.legacyRow(FamilyWraparound, ClassFreeze, L3, ProvenanceCarriedOver)
	if _, err := billing.svc.Downgrade(billing.ctx, DowngradeRequest{Family: FamilyWAL,
		Class: ClassWALBound, To: L0, Actor: "user:2:o@e",
		Reason: "billing's own decision"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []*fixture{orders, billing} {
		adopted, err := f.svc.AdoptLegacy(f.ctx)
		if err != nil {
			t.Fatalf("%s adopt: %v", f.db, err)
		}
		want := 2
		if f == billing {
			want = 1 // billing already decided WAL itself
		}
		if len(adopted) != want {
			t.Fatalf("%s adopted %+v, want %d pairs", f.db, adopted, want)
		}
		again, err := f.svc.AdoptLegacy(f.ctx)
		if err != nil || len(again) != 0 {
			t.Fatalf("%s second adoption = %+v (%v), want none", f.db, again, err)
		}
	}
	for _, c := range []struct {
		f     *fixture
		fam   Family
		class ActionClass
		want  Level
	}{
		{orders, FamilyWAL, ClassWALBound, L2}, {billing, FamilyWAL, ClassWALBound, L0},
		{orders, FamilyLockBlocking, ClassBackendCancel, L0},
		{billing, FamilyLockBlocking, ClassBackendCancel, L0},
		{orders, FamilyWraparound, ClassFreeze, L1}, {billing, FamilyWraparound, ClassFreeze, L1},
	} {
		if got := c.f.granted(c.fam, c.class); got != c.want {
			t.Errorf("%s %s/%s = %v, want %v", c.f.db, c.fam, c.class, got, c.want)
		}
	}
	evs := orders.events(EventFilter{Family: FamilyWAL, Class: ClassWALBound})
	if len(evs) != 1 || evs[0].Type != EventDatabaseScoped || evs[0].Database != orders.db ||
		evs[0].To == nil || *evs[0].To != L2 || evs[0].Actor != ActorPgSage {
		t.Fatalf("adoption event = %+v", evs)
	}
	// The legacy rows stay for databases that bind later.
	var n int
	if err := orders.pool.QueryRow(orders.ctx, `SELECT count(*) FROM sage.sre_family_autonomy
		WHERE deployment_id = $1 AND database_name = ''`,
		orders.store.DeploymentID()).Scan(&n); err != nil || n != 3 {
		t.Fatalf("legacy rows left = %d (%v), want 3", n, err)
	}
}

// An adopted earned level is still capped by the adopting database's own
// evidence at the gate: without its own reviews it acts at L1.
func TestAdoptedPromotionIsCappedByTheDatabasesOwnEvidence(t *testing.T) {
	orders, billing := fleetPair(t)
	orders.legacyRow(FamilyWAL, ClassWALBound, L2, ProvenanceLedger)
	if _, err := billing.svc.AdoptLegacy(billing.ctx); err != nil {
		t.Fatal(err)
	}
	lim := billing.svc.Limiter(Binding{Database: billing.db, HA: &fakeHA{state: HAState{
		Role: RolePrimary}}, Concurrency: &fakeConcurrency{}})
	got, err := lim.Limit(billing.ctx, walBoundRequest(billing.clock.Now()))
	if err != nil || got.Granted != 2 || got.Level != 1 {
		t.Fatalf("adopted L2 without billing's evidence = %+v (%v), want granted 2, level 1",
			got, err)
	}
}

func TestAdoptLegacyWithoutLegacyRowsIsANoOp(t *testing.T) {
	f := newFixture(t)
	adopted, err := f.svc.AdoptLegacy(f.ctx)
	if err != nil || len(adopted) != 0 {
		t.Fatalf("adopt = %+v (%v)", adopted, err)
	}
	if len(f.events(EventFilter{})) != 0 {
		t.Fatal("a no-op adoption wrote history")
	}
}

func TestAdoptLegacyStoreFailureIsAnError(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.svc.AdoptLegacy(ctx); err == nil || errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("cancelled adoption = %v, want a store error", err)
	}
}

// Post-test audit: history recorded before the ledger was per database
// (no database name) is shown with every database's history, and only
// a database filter drops it; a filter naming another database is
// refused.
func TestHistoryShowsDeploymentWideLegacyEvents(t *testing.T) {
	orders, billing := fleetPair(t)
	if _, err := orders.pool.Exec(orders.ctx, `INSERT INTO sage.sre_autonomy_events
		(deployment_id, family, action_class, event_type, actor, reason)
		VALUES ($1, 'wal_retention', 'wal_bound', 'downgraded', 'user:1:a@e',
		        'before database scope')`, orders.store.DeploymentID()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []*fixture{orders, billing} {
		evs := f.events(EventFilter{Family: FamilyWAL})
		if len(evs) != 1 || evs[0].Database != "" || evs[0].Reason != "before database scope" {
			t.Fatalf("%s history = %+v, want the legacy entry", f.db, evs)
		}
		if own := f.events(EventFilter{Family: FamilyWAL, Database: f.db}); len(own) != 0 {
			t.Fatalf("%s own history = %+v, want none", f.db, own)
		}
	}
	if _, err := orders.svc.History(orders.ctx, EventFilter{Database: "billing"}); !errors.Is(
		err, ErrInvalidRequest) {
		t.Fatalf("history filtered to another database = %v", err)
	}
}
