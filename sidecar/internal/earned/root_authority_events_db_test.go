package earned

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Owner addition A against real PostgreSQL: earning or losing model-root
// authority is recorded in the ledger history with the deciding report
// and told to the operator once, whether the change is found by the
// periodic reconcile or at the moment an investigation asks.

type rootNotifier struct {
	mu      sync.Mutex
	changes []RootAuthorityChange
	fail    bool
}

func (n *rootNotifier) NotifyRootAuthority(_ context.Context, c RootAuthorityChange) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.changes = append(n.changes, c)
	if n.fail {
		return errors.New("chat webhook down")
	}
	return nil
}

func (n *rootNotifier) got() []RootAuthorityChange {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]RootAuthorityChange(nil), n.changes...)
}

func (f *fixture) ingestLift(at time.Time, recs ...string) EvalRun {
	f.t.Helper()
	run, err := f.svc.IngestEvalRun(f.ctx, liftReport(at, "live", false, recs...),
		SourceBench, "admin", "")
	if err != nil {
		f.t.Fatalf("ingest lift report: %v", err)
	}
	return run
}

// rootEvents is the database's model-root history, oldest first.
func (f *fixture) rootEvents(family Family) []Event {
	f.t.Helper()
	all, err := f.store.Events(f.ctx, EventFilter{Family: family, Class: ModelRootClass,
		Database: f.db})
	if err != nil {
		f.t.Fatalf("history: %v", err)
	}
	out := []Event{}
	for i := len(all) - 1; i >= 0; i-- {
		out = append(out, all[i])
	}
	return out
}

func TestRootAuthorityEvents_GrantIsRecordedWithTheReportAndToldOnce(t *testing.T) {
	f := newFixture(t)
	n := &rootNotifier{}
	f.svc.WithRootAuthorityNotifier(n)
	run := f.ingestLift(fixtureEpoch.Add(-time.Hour), good("lock_blocking", 16, 16),
		good("wal_retention", 15, 15))
	changes, err := f.svc.ReconcileRootAuthority(f.ctx)
	if err != nil || len(changes) != 1 {
		t.Fatalf("changes = %+v (%v)", changes, err)
	}
	c := changes[0]
	if c.Family != FamilyLockBlocking || !c.Granted || c.ReportID != run.ID ||
		c.Database != "orders" || c.Reason == "" || !c.At.Equal(fixtureEpoch) {
		t.Fatalf("change = %+v", c)
	}
	evs := f.rootEvents(FamilyLockBlocking)
	if len(evs) != 1 || evs[0].Type != EventRootAuthorityGranted ||
		evs[0].Actor != ActorPgSage || evs[0].Database != "orders" {
		t.Fatalf("history = %+v", evs)
	}
	if ev := decodeEvidence(t, evs[0].Evidence); ev["report_id"] != run.ID {
		t.Fatalf("evidence = %v, want report %s", ev, run.ID)
	}
	if got := n.got(); len(got) != 1 || got[0].ReportID != run.ID || !got[0].Granted {
		t.Fatalf("notified = %+v", got)
	}
	again, err := f.svc.ReconcileRootAuthority(f.ctx)
	if err != nil || len(again) != 0 || len(f.rootEvents(FamilyLockBlocking)) != 1 ||
		len(n.got()) != 1 {
		t.Fatalf("a second reconcile repeated the change: %+v (%v)", again, err)
	}
	if evs := f.rootEvents(FamilyWAL); len(evs) != 0 {
		t.Fatalf("wal_retention never earned authority but has history %+v", evs)
	}
}

func TestRootAuthorityEvents_NothingEarnedNothingRecorded(t *testing.T) {
	f := newFixture(t)
	n := &rootNotifier{}
	f.svc.WithRootAuthorityNotifier(n)
	if changes, err := f.svc.ReconcileRootAuthority(f.ctx); err != nil || len(changes) != 0 {
		t.Fatalf("with no report: %+v (%v)", changes, err)
	}
	f.ingestLift(fixtureEpoch.Add(-time.Hour), good("lock_blocking", 15, 15))
	if changes, err := f.svc.ReconcileRootAuthority(f.ctx); err != nil || len(changes) != 0 {
		t.Fatalf("with 15/15: %+v (%v)", changes, err)
	}
	for _, fam := range Families() {
		if evs := f.rootEvents(fam); len(evs) != 0 {
			t.Fatalf("%s: history %+v", fam, evs)
		}
	}
	if len(n.got()) != 0 {
		t.Fatalf("notified %+v", n.got())
	}
}

func TestRootAuthorityEvents_LosingItIsRecordedWithTheNewReport(t *testing.T) {
	f := newFixture(t)
	n := &rootNotifier{}
	f.svc.WithRootAuthorityNotifier(n)
	f.ingestLift(fixtureEpoch.Add(-3*time.Hour), good("lock_blocking", 40, 40))
	if _, err := f.svc.ReconcileRootAuthority(f.ctx); err != nil {
		t.Fatal(err)
	}
	worse := f.ingestLift(fixtureEpoch.Add(-time.Hour), good("lock_blocking", 2, 20))
	changes, err := f.svc.ReconcileRootAuthority(f.ctx)
	if err != nil || len(changes) != 1 || changes[0].Granted ||
		changes[0].ReportID != worse.ID {
		t.Fatalf("changes = %+v (%v)", changes, err)
	}
	evs := f.rootEvents(FamilyLockBlocking)
	if len(evs) != 2 || evs[1].Type != EventRootAuthorityRevoked ||
		!strings.Contains(evs[1].Reason, "lower bound") {
		t.Fatalf("history = %+v", evs)
	}
	if got := n.got(); len(got) != 2 || got[1].Granted {
		t.Fatalf("notified = %+v", got)
	}
}

// A grant whose report ages out is revoked by the periodic reconcile.
func TestRootAuthorityEvents_AStaleMeasurementLosesIt(t *testing.T) {
	f := newFixture(t)
	f.ingestLift(fixtureEpoch.Add(-time.Hour), good("lock_blocking", 40, 40))
	if _, err := f.svc.ReconcileRootAuthority(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(f.svc.cfg.Thresholds.BenchMaxAge + time.Hour)
	changes, err := f.svc.ReconcileRootAuthority(f.ctx)
	if err != nil || len(changes) != 1 || changes[0].Granted ||
		!strings.Contains(changes[0].Reason, "old") {
		t.Fatalf("changes = %+v (%v)", changes, err)
	}
	if evs := f.rootEvents(FamilyLockBlocking); len(evs) != 2 ||
		evs[1].Type != EventRootAuthorityRevoked {
		t.Fatalf("history = %+v", evs)
	}
}

func TestRootAuthorityEvents_EarnedBackAfterLosingIt(t *testing.T) {
	f := newFixture(t)
	for i, k := range []int{40, 2, 40} {
		f.ingestLift(fixtureEpoch.Add(time.Duration(i-5)*time.Hour),
			good("lock_blocking", k, 40))
		if _, err := f.svc.ReconcileRootAuthority(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	evs := f.rootEvents(FamilyLockBlocking)
	want := []EventType{EventRootAuthorityGranted, EventRootAuthorityRevoked,
		EventRootAuthorityGranted}
	if len(evs) != len(want) {
		t.Fatalf("history = %+v", evs)
	}
	for i, e := range evs {
		if e.Type != want[i] {
			t.Fatalf("history[%d] = %s, want %s", i, e.Type, want[i])
		}
	}
}

// An investigation that asks is never granted authority the history does
// not show: the grant is recorded (and told) before it is used.
func TestRootAuthorityEvents_AskingRecordsTheGrantBeforeUsingIt(t *testing.T) {
	f := newFixture(t)
	n := &rootNotifier{}
	f.svc.WithRootAuthorityNotifier(n)
	run := f.ingestLift(fixtureEpoch.Add(-time.Hour), good("lock_blocking", 16, 16))
	got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking)
	if err != nil || !got.Granted {
		t.Fatalf("authority = %+v (%v)", got, err)
	}
	evs := f.rootEvents(FamilyLockBlocking)
	if len(evs) != 1 || evs[0].Type != EventRootAuthorityGranted ||
		decodeEvidence(t, evs[0].Evidence)["report_id"] != run.ID {
		t.Fatalf("history = %+v", evs)
	}
	if _, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking); err != nil {
		t.Fatal(err)
	}
	if len(f.rootEvents(FamilyLockBlocking)) != 1 || len(n.got()) != 1 {
		t.Fatalf("asking again repeated the grant: %d told", len(n.got()))
	}
	if evs := f.rootEvents(FamilyWAL); len(evs) != 0 {
		t.Fatalf("asking about lock_blocking recorded wal_retention: %+v", evs)
	}
}

// The ledger cannot record the grant: the investigation is not granted.
func TestRootAuthorityEvents_UnrecordableGrantIsNotGranted(t *testing.T) {
	f := newFixture(t)
	f.ingestLift(fixtureEpoch.Add(-time.Hour), good("lock_blocking", 16, 16))
	f.rejectRootEvents()
	got, err := f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking)
	if err == nil || got.Granted || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("authority = %+v (%v), want ErrUnavailable and no grant", got, err)
	}
}

// rejectRootEvents makes the history refuse this deployment's model-root
// entries until the test ends.
func (f *fixture) rejectRootEvents() {
	f.t.Helper()
	name := "reject_root_" + strings.ReplaceAll(f.store.DeploymentID(), "-", "")
	_, err := f.pool.Exec(f.ctx, `CREATE FUNCTION public.`+name+`() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN
		IF NEW.deployment_id = '`+f.store.DeploymentID()+`'::uuid
		   AND NEW.action_class = 'model_root' THEN
			RAISE EXCEPTION 'history refused';
		END IF;
		RETURN NEW; END $$;
		CREATE TRIGGER `+name+` BEFORE INSERT ON sage.sre_autonomy_events
		FOR EACH ROW EXECUTE FUNCTION public.`+name+`()`)
	if err != nil {
		f.t.Fatalf("install the refusing trigger: %v", err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+name+
			` ON sage.sre_autonomy_events; DROP FUNCTION IF EXISTS public.`+name+`()`)
	})
}

func TestRootAuthorityEvents_ConcurrentAsksRecordOnce(t *testing.T) {
	f := newFixture(t)
	n := &rootNotifier{}
	f.svc.WithRootAuthorityNotifier(n)
	f.ingestLift(fixtureEpoch.Add(-time.Hour), good("lock_blocking", 16, 16))
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, errs[i] = f.svc.ReconcileRootAuthority(f.ctx)
				return
			}
			_, errs[i] = f.svc.ModelRootAuthority(f.ctx, FamilyLockBlocking)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if evs := f.rootEvents(FamilyLockBlocking); len(evs) != 1 || len(n.got()) != 1 {
		t.Fatalf("history %d events, told %d times; want 1 and 1", len(evs), len(n.got()))
	}
}

// A failed notification keeps the recorded change and is reported; it is
// not told twice.
func TestRootAuthorityEvents_NotifierFailureKeepsTheEvent(t *testing.T) {
	f := newFixture(t)
	n := &rootNotifier{fail: true}
	f.svc.WithRootAuthorityNotifier(n)
	f.ingestLift(fixtureEpoch.Add(-time.Hour), good("lock_blocking", 16, 16))
	changes, err := f.svc.ReconcileRootAuthority(f.ctx)
	if err == nil || !strings.Contains(err.Error(), "chat webhook down") || len(changes) != 1 {
		t.Fatalf("changes = %+v, err = %v", changes, err)
	}
	if evs := f.rootEvents(FamilyLockBlocking); len(evs) != 1 {
		t.Fatalf("history = %+v", evs)
	}
	if _, err := f.svc.ReconcileRootAuthority(f.ctx); err != nil || len(n.got()) != 1 {
		t.Fatalf("second reconcile: %v, told %d times", err, len(n.got()))
	}
}

func TestRootAuthorityEvents_WithoutANotifierStillRecorded(t *testing.T) {
	f := newFixture(t)
	f.ingestLift(fixtureEpoch.Add(-time.Hour), good("lock_blocking", 16, 16))
	changes, err := f.svc.ReconcileRootAuthority(f.ctx)
	if err != nil || len(changes) != 1 || len(f.rootEvents(FamilyLockBlocking)) != 1 {
		t.Fatalf("changes = %+v (%v)", changes, err)
	}
}

// Each database keeps its own history. A bench report measures the
// build, so it counts for every database of the deployment; each
// database records (and tells) the grant in its own history, once, and
// one database's entry never stands in for another's.
func TestRootAuthorityEvents_PerDatabase(t *testing.T) {
	dep := newUUID(t)
	orders := newFixtureFor(t, dep, "orders")
	billing := newFixtureFor(t, dep, "billing")
	orders.ingestLift(fixtureEpoch.Add(-time.Hour), good("lock_blocking", 16, 16))
	if _, err := orders.svc.ReconcileRootAuthority(orders.ctx); err != nil {
		t.Fatal(err)
	}
	if evs := billing.rootEvents(FamilyLockBlocking); len(evs) != 0 {
		t.Fatalf("orders' entry stands in billing's history: %+v", evs)
	}
	changes, err := billing.svc.ReconcileRootAuthority(billing.ctx)
	if err != nil || len(changes) != 1 || changes[0].Database != "billing" {
		t.Fatalf("billing changes = %+v (%v)", changes, err)
	}
	if evs := billing.rootEvents(FamilyLockBlocking); len(evs) != 1 ||
		evs[0].Database != "billing" {
		t.Fatalf("billing history = %+v", evs)
	}
	if evs := orders.rootEvents(FamilyLockBlocking); len(evs) != 1 ||
		evs[0].Database != "orders" {
		t.Fatalf("orders history = %+v", evs)
	}
}

func TestModelLiftView_CarriesOverridesNeeded(t *testing.T) {
	f := newFixture(t)
	f.ingestLift(fixtureEpoch.Add(-time.Hour), good("lock_blocking", 15, 15))
	view, err := f.svc.ModelLiftView(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range view.Families {
		want := 16
		if a.Family == FamilyLockBlocking {
			want = 1
		}
		if a.OverridesNeeded != want {
			t.Errorf("%s needs %d, want %d", a.Family, a.OverridesNeeded, want)
		}
	}
	if evs := f.rootEvents(FamilyLockBlocking); len(evs) != 0 {
		t.Fatalf("reading the view recorded history: %+v", evs)
	}
}
