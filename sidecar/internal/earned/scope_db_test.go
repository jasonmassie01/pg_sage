package earned

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// P0-5: the earned-autonomy ledger is per database. In a fleet sharing
// one control database (one deployment), one database's shadow reviews,
// live outcomes, safety violations, proposals, approvals, downgrades,
// history, game days and carried-over pairs never move another
// database's level. Bench evidence is about pg_sage itself and stays
// deployment-wide.

// fleetPair is two databases of one deployment on one clock.
func fleetPair(t *testing.T) (orders, billing *fixture) {
	t.Helper()
	orders = newFixtureFor(t, newUUID(t), "orders")
	return orders, orders.sibling("billing")
}

func TestFleetOneDatabasesEvidenceDoesNotPromoteAnother(t *testing.T) {
	orders, billing := fleetPair(t)
	orders.seedL2Evidence(FamilyWAL, 25, 0)
	created, err := billing.svc.ProposePromotions(billing.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range created {
		if p.Family == FamilyWAL {
			t.Fatalf("billing proposed from orders' reviews: %+v", p)
		}
	}
	st := orders.promote(FamilyWAL, ClassWALBound)
	if st.Level != L2 {
		t.Fatalf("orders after approval = %+v", st)
	}
	if got := billing.granted(FamilyWAL, ClassWALBound); got != L1 {
		t.Fatalf("billing level after orders' promotion = %v, want L1", got)
	}
	sh, err := billing.store.ShadowStats(billing.ctx, FamilyWAL, time.Time{})
	if err != nil || sh.Reviewed != 0 || !sh.FirstReviewAt.IsZero() {
		t.Fatalf("billing shadow = %+v (%v), want none", sh, err)
	}
	ev, err := billing.svc.Evidence(billing.ctx, FamilyWAL, ClassWALBound)
	if err != nil {
		t.Fatal(err)
	}
	// The bench report orders ingested is deployment-wide evidence.
	if ev.Bench == nil || ev.Shadow.Reviewed != 0 {
		t.Fatalf("billing evidence = %+v: bench must be shared, reviews must not", ev)
	}
	a := Assess(billing.svc.cfg.Thresholds, L2, ev)
	if checkMet(a, "bench_present") != true || checkMet(a, "shadow_volume") != false {
		t.Fatalf("billing assessment = %+v", a.Checks)
	}
}

func checkMet(a Assessment, name string) any {
	for _, c := range a.Checks {
		if c.Name == name {
			return c.Met
		}
	}
	return "missing"
}

func TestFleetLiveOutcomesAndViolationsStayInTheirDatabase(t *testing.T) {
	orders, billing := fleetPair(t)
	orders.seedL3()
	billing.seedL2Recoveries(FamilyWraparound, ClassFreeze, 3)
	live, err := orders.store.LiveStats(orders.ctx, FamilyWraparound, ClassFreeze)
	if err != nil || live.VerifiedL2 != 50 {
		t.Fatalf("orders live = %+v (%v), want its own 50 recoveries", live, err)
	}
	if err := billing.svc.RecordOutcome(billing.ctx, Outcome{Database: billing.db,
		Family: FamilyWraparound, Class: ClassVacuum, Level: L1, Result: ResultHarmful,
		Source: SourceOperator, Actor: "user:2:o@e", Detail: "billing only"}); err != nil {
		t.Fatal(err)
	}
	if got := orders.granted(FamilyWraparound, ClassFreeze); got != L3 {
		t.Fatalf("billing's harmful outcome demoted orders to %v", got)
	}
	n, err := orders.store.FamilyViolations(orders.ctx, FamilyWraparound, time.Time{})
	if err != nil || n != 0 {
		t.Fatalf("orders violations = %d (%v), want 0", n, err)
	}
	if n, err = billing.store.FamilyViolations(billing.ctx, FamilyWraparound,
		time.Time{}); err != nil || n != 1 {
		t.Fatalf("billing violations = %d (%v), want 1", n, err)
	}
	ordersLim := orders.svc.Limiter(Binding{Database: orders.db, HA: &fakeHA{
		state: HAState{Role: RolePrimary}}, Concurrency: &fakeConcurrency{}})
	got, err := ordersLim.Limit(orders.ctx, freezeRequest(orders.clock.Now()))
	if err != nil || got.Level != 3 || got.Downgraded {
		t.Fatalf("orders limit after billing's violation = %+v (%v)", got, err)
	}
}

func TestFleetProposalsApprovalsAndDowngradesAreScoped(t *testing.T) {
	orders, billing := fleetPair(t)
	orders.seedL2Evidence(FamilyWAL, 25, 0)
	if _, err := orders.svc.ProposePromotions(orders.ctx); err != nil {
		t.Fatal(err)
	}
	p := orders.pending(FamilyWAL, ClassWALBound)
	if p == nil {
		t.Fatal("orders has no pending WAL proposal")
	}
	if billing.pending(FamilyWAL, ClassWALBound) != nil {
		t.Fatal("billing lists orders' proposal")
	}
	if _, err := billing.svc.Proposal(billing.ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("billing read orders' proposal: %v", err)
	}
	if _, err := billing.svc.Approve(billing.ctx, p.ID, "user:1:admin@example.com",
		"wrong database"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("approval through billing = %v, want not found", err)
	}
	if _, err := billing.svc.Reject(billing.ctx, p.ID, "user:1:a@e", "no"); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("reject through billing = %v, want not found", err)
	}
	if billing.granted(FamilyWAL, ClassWALBound) != L1 ||
		orders.pending(FamilyWAL, ClassWALBound) == nil {
		t.Fatal("a refused cross-database decision changed state")
	}
	if _, err := billing.svc.Downgrade(billing.ctx, DowngradeRequest{Family: FamilyWAL,
		Class: AllClasses, To: L0, Actor: "user:2:o@e", Reason: "billing drill"}); err != nil {
		t.Fatal(err)
	}
	if orders.pending(FamilyWAL, ClassWALBound) == nil {
		t.Fatal("billing's downgrade superseded orders' proposal")
	}
	if got := orders.granted(FamilyWAL, ClassWALBound); got != L1 {
		t.Fatalf("billing's downgrade moved orders to %v", got)
	}
	for _, e := range orders.events(EventFilter{Family: FamilyWAL}) {
		if e.Type == EventDowngraded {
			t.Fatalf("orders' history shows billing's downgrade: %+v", e)
		}
		if e.Database != orders.db {
			t.Fatalf("orders' event names %q", e.Database)
		}
	}
	evs := billing.events(EventFilter{Family: FamilyWAL})
	if len(evs) == 0 || evs[0].Type != EventDowngraded || evs[0].Database != billing.db {
		t.Fatalf("billing history = %+v", evs)
	}
}

func TestFleetGameDaysCountForTheirDatabaseOnly(t *testing.T) {
	orders, billing := fleetPair(t)
	if _, err := orders.svc.IngestEvalRun(orders.ctx, benchReport(fixtureEpoch.Add(
		-time.Hour), FamilyWraparound), SourceGameDay, ActorPgSage, orders.db); err != nil {
		t.Fatal(err)
	}
	if _, err := orders.svc.IngestEvalRun(orders.ctx, benchReport(fixtureEpoch.Add(
		-2*time.Hour), FamilyWAL), SourceGameDay, ActorPgSage, billing.db); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("a game day of billing through orders' ledger: %v", err)
	}
	ev, err := billing.svc.Evidence(billing.ctx, FamilyWraparound, ClassFreeze)
	if err != nil || len(ev.GameDays) != 0 {
		t.Fatalf("billing game days = %+v (%v), want none", ev.GameDays, err)
	}
	if ev, err = orders.svc.Evidence(orders.ctx, FamilyWraparound, ClassFreeze); err != nil ||
		len(ev.GameDays) != 1 {
		t.Fatalf("orders game days = %+v (%v), want 1", ev.GameDays, err)
	}
}

func TestFleetWritesNamingAnotherDatabaseAreRefused(t *testing.T) {
	orders, _ := fleetPair(t)
	err := orders.svc.RecordOutcome(orders.ctx, Outcome{Database: "billing",
		Family: FamilyWAL, Class: ClassWALBound, Level: L2, Result: ResultHarmful,
		Source: SourceOperator, Actor: "user:2:o@e"})
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "billing") {
		t.Fatalf("outcome of billing through orders = %v", err)
	}
	err = orders.svc.RecordReview(orders.ctx, Review{Database: "billing",
		InvestigationID: newUUID(t), Family: FamilyWAL, Verdict: VerdictAccepted,
		Reviewer: "user:2:o@e"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("review of billing through orders = %v", err)
	}
	if _, err := orders.svc.SeedCarriedOver(orders.ctx, "billing",
		autonomousBound()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("carry-over of billing through orders = %v", err)
	}
	lim := orders.svc.Limiter(Binding{Database: "billing"})
	if _, err := lim.Limit(orders.ctx, freezeRequest(orders.clock.Now())); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("a limiter bound to another database must fail closed: %v", err)
	}
	if orders.svc.Database() != "orders" || orders.store.Database() != "orders" {
		t.Fatalf("scope = %q/%q", orders.svc.Database(), orders.store.Database())
	}
}

func TestStoreNeedsADatabase(t *testing.T) {
	pool := testPool(t)
	for _, db := range []string{"", "   ", strings.Repeat("d", 201)} {
		if _, err := NewPostgresStore(pool, newUUID(t), db); !errors.Is(err,
			ErrInvalidRequest) {
			t.Errorf("database %q: %v", db, err)
		}
	}
	st, err := NewPostgresStore(pool, newUUID(t), strings.Repeat("d", 200))
	if err != nil || len(st.Database()) != 200 {
		t.Fatalf("200-character database: %v", err)
	}
}

// Reviews arriving for two databases at once are each counted exactly
// once, in their own database.
func TestFleetConcurrentReviewsStayInTheirDatabase(t *testing.T) {
	orders, billing := fleetPair(t)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 20; i++ {
		for _, f := range []*fixture{orders, billing} {
			wg.Add(1)
			go func(f *fixture, i int) {
				defer wg.Done()
				verdict := VerdictAccepted
				if f == billing && i%2 == 0 {
					verdict = VerdictRejected
				}
				errs <- f.svc.RecordReview(f.ctx, Review{Database: f.db,
					InvestigationID: newUUID(t), Family: FamilyLockBlocking,
					Verdict: verdict, Reviewer: fmt.Sprintf("user:%d:o@e", i)})
			}(f, i)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for f, want := range map[*fixture]Shadow{orders: {Reviewed: 20, Accepted: 20},
		billing: {Reviewed: 20, Accepted: 10}} {
		sh, err := f.store.ShadowStats(f.ctx, FamilyLockBlocking, time.Time{})
		if err != nil || sh.Reviewed != want.Reviewed || sh.Accepted != want.Accepted {
			t.Errorf("%s shadow = %+v (%v), want %+v", f.db, sh, err, want)
		}
	}
}

// The failover cooldown is the database's own: a role change on billing
// caps billing, not orders.
func TestFleetFailoverCooldownIsPerDatabase(t *testing.T) {
	orders, billing := fleetPair(t)
	orders.seedL3()
	billing.seedL3()
	now := orders.clock.Now()
	limit := func(f *fixture, changed time.Time) Level {
		t.Helper()
		lim := f.svc.Limiter(Binding{Database: f.db, HA: &fakeHA{state: HAState{
			Role: RolePrimary, LastRoleChange: changed}}, Concurrency: &fakeConcurrency{}})
		got, err := lim.Limit(f.ctx, freezeRequest(now))
		if err != nil {
			t.Fatal(err)
		}
		return Level(got.Level)
	}
	if got := limit(billing, now.Add(-time.Minute)); got != L1 {
		t.Fatalf("billing right after its failover = %v, want L1", got)
	}
	if got := limit(orders, time.Time{}); got != L3 {
		t.Fatalf("orders without a failover = %v, want L3", got)
	}
}

// Reviews are trust evidence: pg_sage itself, internal actors and an
// unbound MCP agent cannot record them; an authenticated MCP principal
// can, and stays named.
func TestReviewerMustBeAnAuthenticatedPrincipal(t *testing.T) {
	f := newFixture(t)
	for _, reviewer := range []string{ActorPgSage, "system", "mcp-agent", "  "} {
		err := f.svc.RecordReview(f.ctx, Review{Database: f.db, InvestigationID: newUUID(t),
			Family: FamilyLockBlocking, Verdict: VerdictAccepted, Reviewer: reviewer})
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("reviewer %q: %v", reviewer, err)
		}
	}
	for _, reviewer := range []string{"user:7:o@e", "mcp:user:7"} {
		if err := f.svc.RecordReview(f.ctx, Review{Database: f.db,
			InvestigationID: newUUID(t), Family: FamilyLockBlocking,
			Verdict: VerdictAccepted, Reviewer: reviewer}); err != nil {
			t.Errorf("reviewer %q refused: %v", reviewer, err)
		}
	}
	sh, err := f.store.ShadowStats(f.ctx, FamilyLockBlocking, time.Time{})
	if err != nil || sh.Reviewed != 2 {
		t.Fatalf("shadow = %+v (%v), want the two accepted reviewers", sh, err)
	}
}
