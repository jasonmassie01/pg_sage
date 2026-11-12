package earned

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// earned-06 against PostgreSQL: an L3 incident pair whose bench report
// ages out is capped by the evidence sweep without any action being
// requested, recorded once (across restarts and concurrent gates), warned
// about once a week ahead, and cleared when a new report arrives.

// decayGenerated is when the seeded bench report was generated
// (seedL2Evidence: an hour before the fixture epoch).
var decayGenerated = fixtureEpoch.Add(-time.Hour)

type expiryRecorder struct {
	mu    sync.Mutex
	warns []ExpiryWarning
	err   error
}

func (n *expiryRecorder) NotifyEvidenceExpiring(_ context.Context, w ExpiryWarning) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.warns = append(n.warns, w)
	return n.err
}

func (n *expiryRecorder) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.warns)
}

// newDecayFixture is an L3 wraparound/freeze pair whose shadow window
// stays full past the bench report's 30 days: 20 more accepted reviews
// ten days after the epoch, so only bench_fresh decays.
func newDecayFixture(t *testing.T) *limiterFixture {
	t.Helper()
	lf := newLimiterFixture(t)
	lf.seedL3()
	lf.clock.Set(fixtureEpoch.Add(10 * day))
	for i := 0; i < 20; i++ {
		if err := lf.svc.RecordReview(lf.ctx, Review{Database: lf.db,
			InvestigationID: newUUID(t), Family: FamilyWraparound, Verdict: VerdictAccepted,
			Reviewer: "user:7:ops@example.com"}); err != nil {
			t.Fatalf("review: %v", err)
		}
	}
	return lf
}

func (lf *limiterFixture) sweep(n ExpiryNotifier) SweepResult {
	lf.t.Helper()
	res, err := lf.lim.Sweep(lf.ctx, n)
	if err != nil {
		lf.t.Fatalf("Sweep: %v", err)
	}
	return res
}

// pairEvents are the freeze pair's history entries of typ, newest first.
func (lf *limiterFixture) pairEvents(typ EventType) []Event {
	lf.t.Helper()
	var out []Event
	for _, e := range lf.events(EventFilter{Family: FamilyWraparound, Class: ClassFreeze,
		Database: lf.db, Limit: MaxEventPage}) {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (lf *limiterFixture) decayedReasons() int {
	lf.t.Helper()
	var n int
	if err := lf.pool.QueryRow(lf.ctx, `SELECT count(*) FROM sage.sre_autonomy_events
		WHERE deployment_id = $1 AND reason LIKE '%evidence_decayed%'`,
		lf.store.DeploymentID()).Scan(&n); err != nil {
		lf.t.Fatal(err)
	}
	return n
}

func TestDecaySweepCapsOnceAndClearsOnANewReport(t *testing.T) {
	lf := newDecayFixture(t)
	lf.clock.Set(decayGenerated.Add(30*day + time.Minute))
	if res := lf.sweep(nil); res.Capped != 1 || res.Pairs < 1 {
		t.Fatalf("first sweep = %+v, want one cap", res)
	}
	capped := lf.pairEvents(EventCapped)
	if len(capped) != 1 || !strings.Contains(capped[0].Reason, "evidence_decayed:bench_fresh") {
		t.Fatalf("capped events = %+v", capped)
	}
	var ev struct {
		Granted   Level    `json:"granted"`
		Supported Level    `json:"supported"`
		Failing   []string `json:"failing_checks"`
	}
	if err := json.Unmarshal(capped[0].Evidence, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Granted != L3 || ev.Supported != L1 || !slices.Equal(ev.Failing,
		[]string{"bench_fresh"}) {
		t.Fatalf("cap evidence = %s", capped[0].Evidence)
	}
	if res := lf.sweep(nil); res.Capped != 0 {
		t.Fatalf("second sweep = %+v, want no new cap", res)
	}
	if got := lf.limit(freezeRequest(lf.clock.Now())); got.Level != 1 || got.Granted != 3 {
		t.Fatalf("limit after decay = %+v, want L1 of granted L3", got)
	}
	if n := len(lf.pairEvents(EventCapped)); n != 1 {
		t.Fatalf("Limit wrote another cap: %d", n)
	}
	// CHECK-19.
	if n := lf.decayedReasons(); n != 1 {
		t.Fatalf("evidence_decayed events = %d, want 1", n)
	}
	if _, err := lf.svc.IngestEvalRun(lf.ctx, benchReport(lf.clock.Now(), FamilyWraparound),
		SourceBench, "user:1:admin@example.com", ""); err != nil {
		t.Fatal(err)
	}
	if res := lf.sweep(nil); res.Cleared != 1 {
		t.Fatalf("sweep after a fresh report = %+v, want one clear", res)
	}
	if n := len(lf.pairEvents(EventCapCleared)); n != 1 {
		t.Fatalf("cap_cleared events = %d", n)
	}
	if got := lf.limit(freezeRequest(lf.clock.Now())); got.Level != 3 {
		t.Fatalf("limit after a fresh report = %+v, want L3", got)
	}
}

func TestDecayAtExactlyThirtyDaysIsNoCap(t *testing.T) {
	lf := newDecayFixture(t)
	lf.clock.Set(decayGenerated.Add(30 * day))
	at, checks, err := lf.svc.EvidenceExpiry(lf.ctx, FamilyWraparound, ClassFreeze)
	if err != nil || !at.Equal(decayGenerated.Add(30*day)) ||
		!slices.Equal(checks, []string{"bench_fresh"}) {
		t.Fatalf("expiry = %v %v %v", at, checks, err)
	}
	if res := lf.sweep(nil); res.Capped != 0 {
		t.Fatalf("sweep at 30 days = %+v", res)
	}
	if got := lf.limit(freezeRequest(lf.clock.Now())); got.Level != 3 {
		t.Fatalf("limit at 30 days = %+v, want L3", got)
	}
	if n := len(lf.pairEvents(EventCapped)); n != 0 {
		t.Fatalf("capped events at 30 days = %d", n)
	}
}

func TestDecayWarningIsSentOnceSevenDaysAhead(t *testing.T) {
	lf := newDecayFixture(t)
	n := &expiryRecorder{}
	lf.clock.Set(decayGenerated.Add(23*day - time.Minute))
	if res := lf.sweep(n); res.Warned != 0 || n.count() != 0 {
		t.Fatalf("7 days + 1 min ahead: %+v, %d warnings", res, n.count())
	}
	lf.clock.Set(decayGenerated.Add(23*day + time.Minute))
	if res := lf.sweep(n); res.Warned != 1 || n.count() != 1 {
		t.Fatalf("7 days - 1 min ahead: %+v, %d warnings", res, n.count())
	}
	w := n.warns[0]
	if w.Family != FamilyWraparound || w.Class != ClassFreeze || w.Level != L3 ||
		w.Database != "orders" || !w.HoldsUntil.Equal(decayGenerated.Add(30*day)) ||
		!slices.Equal(w.Checks, []string{"bench_fresh"}) {
		t.Fatalf("warning = %+v", w)
	}
	lf.sweep(n)
	restarted := lf.svc.Limiter(lf.lim.b)
	if _, err := restarted.Sweep(lf.ctx, n); err != nil {
		t.Fatal(err)
	}
	if n.count() != 1 {
		t.Fatalf("warnings after a second sweep and a restart = %d, want 1", n.count())
	}
	evs := lf.pairEvents(EventEvidenceExpiring)
	if len(evs) != 1 || !strings.Contains(string(evs[0].Evidence), "bench_fresh") {
		t.Fatalf("expiry events = %+v", evs)
	}
	if n := len(lf.pairEvents(EventCapped)); n != 0 {
		t.Fatalf("a warning is not a cap: %d capped", n)
	}
}

// A notification that fails stays recorded and is not retold.
func TestDecayWarningNotifyFailureIsReturned(t *testing.T) {
	lf := newDecayFixture(t)
	n := &expiryRecorder{err: errors.New("smtp down")}
	lf.clock.Set(decayGenerated.Add(25 * day))
	_, err := lf.lim.Sweep(lf.ctx, n)
	if err == nil || !strings.Contains(err.Error(), "smtp down") {
		t.Fatalf("sweep error = %v, want the notify failure", err)
	}
	if len(lf.pairEvents(EventEvidenceExpiring)) != 1 {
		t.Fatal("the warning was not recorded")
	}
	n.err = nil
	lf.sweep(n)
	if n.count() != 1 {
		t.Fatalf("notify calls = %d, want 1", n.count())
	}
}

func TestDecayRestartDoesNotDuplicateTheCap(t *testing.T) {
	lf := newDecayFixture(t)
	lf.clock.Set(decayGenerated.Add(31 * day))
	lf.sweep(nil)
	restarted := lf.svc.Limiter(lf.lim.b)
	res, err := restarted.Sweep(lf.ctx, nil)
	if err != nil || res.Capped != 0 {
		t.Fatalf("restarted sweep = %+v %v", res, err)
	}
	if _, err := restarted.Limit(lf.ctx, freezeRequest(lf.clock.Now())); err != nil {
		t.Fatal(err)
	}
	if n := len(lf.pairEvents(EventCapped)); n != 1 {
		t.Fatalf("capped events after a restart = %d, want 1", n)
	}
}

// failCapInserts makes every cap insert of f's deployment fail until the
// returned func runs.
func failCapInserts(t *testing.T, lf *limiterFixture) func() {
	t.Helper()
	name := "decay_fail_" + strings.ReplaceAll(newUUID(t), "-", "")
	ddl := fmt.Sprintf(`CREATE FUNCTION sage.%[1]s() RETURNS trigger LANGUAGE plpgsql AS $f$
		BEGIN
			IF NEW.deployment_id = '%[2]s' AND NEW.event_type = 'capped' THEN
				RAISE EXCEPTION 'injected cap insert failure';
			END IF;
			RETURN NEW;
		END $f$;
		CREATE TRIGGER %[1]s BEFORE INSERT ON sage.sre_autonomy_events
			FOR EACH ROW EXECUTE FUNCTION sage.%[1]s();`, name, lf.store.DeploymentID())
	if _, err := lf.pool.Exec(lf.ctx, ddl); err != nil {
		t.Fatalf("install failing trigger: %v", err)
	}
	drop := func() {
		_, _ = lf.pool.Exec(context.Background(), fmt.Sprintf(
			`DROP TRIGGER IF EXISTS %[1]s ON sage.sre_autonomy_events;
			 DROP FUNCTION IF EXISTS sage.%[1]s()`, name))
	}
	t.Cleanup(drop)
	return drop
}

func TestDecayFailedCapInsertIsLoggedAndRetried(t *testing.T) {
	lf := newDecayFixture(t)
	var mu sync.Mutex
	var logs []string
	lf.svc.cfg.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	lf.clock.Set(decayGenerated.Add(31 * day))
	restore := failCapInserts(t, lf)
	if res, err := lf.lim.Sweep(lf.ctx, nil); err != nil || res.Capped != 0 {
		t.Fatalf("sweep with a failing insert = %+v %v", res, err)
	}
	mu.Lock()
	logged := strings.Join(logs, "\n")
	mu.Unlock()
	if !strings.Contains(logged, "record capped") || !strings.Contains(logged,
		"injected cap insert failure") {
		t.Fatalf("log = %q", logged)
	}
	if got := lf.limit(freezeRequest(lf.clock.Now())); got.Level != 1 {
		t.Fatalf("an unrecorded cap must still cap: %+v", got)
	}
	restore()
	if res := lf.sweep(nil); res.Capped != 1 {
		t.Fatalf("retry sweep = %+v, want the cap recorded", res)
	}
	if n := len(lf.pairEvents(EventCapped)); n != 1 {
		t.Fatalf("capped events = %d", n)
	}
}

func TestDecayWithoutABenchReportHasNoExpiry(t *testing.T) {
	lf := newLimiterFixture(t)
	at, checks, err := lf.svc.EvidenceExpiry(lf.ctx, FamilyWraparound, ClassFreeze)
	if err != nil || !at.IsZero() || len(checks) != 0 {
		t.Fatalf("expiry without evidence = %v %v %v", at, checks, err)
	}
	n := &expiryRecorder{}
	if res := lf.sweep(n); res != (SweepResult{}) || n.count() != 0 {
		t.Fatalf("sweep of a fresh ledger = %+v, %d warnings", res, n.count())
	}
	at, checks, err = lf.svc.EvidenceExpiry(lf.ctx, FamilyTuning, ClassIndexCreate)
	if err != nil || !at.IsZero() || checks != nil {
		t.Fatalf("self-initiated expiry = %v %v %v", at, checks, err)
	}
}

func TestDecayEvidenceExpiryReadFailureIsAnError(t *testing.T) {
	lf := newDecayFixture(t)
	ctx, cancel := context.WithCancel(lf.ctx)
	cancel()
	if _, _, err := lf.svc.EvidenceExpiry(ctx, FamilyWraparound, ClassFreeze); err == nil {
		t.Fatal("a failed read must be an error, not a zero expiry")
	}
	if _, err := lf.lim.Sweep(ctx, nil); err == nil {
		t.Fatal("a sweep that cannot read the ledger must fail")
	}
}

func TestDecayCarriedOverPairsAreSkipped(t *testing.T) {
	lf := newLimiterFixture(t)
	if _, err := lf.svc.SeedCarriedOver(lf.ctx, "orders", autonomousBound()); err != nil {
		t.Fatal(err)
	}
	lf.clock.Advance(40 * day)
	n := &expiryRecorder{}
	if res := lf.sweep(n); res.Capped != 0 || res.Warned != 0 || res.Pairs != 0 {
		t.Fatalf("sweep of carried pairs = %+v", res)
	}
	if got := lf.limit(freezeRequest(lf.clock.Now())); got.Level != 3 {
		t.Fatalf("carried pair = %+v, want L3", got)
	}
	if n := len(lf.pairEvents(EventCapped)); n != 0 {
		t.Fatalf("a carried pair recorded %d decay caps", n)
	}
}

// Sweep and Limit racing on one decayed pair record one cap.
func TestDecayConcurrentSweepAndLimitWriteOneEvent(t *testing.T) {
	lf := newDecayFixture(t)
	lf.clock.Set(decayGenerated.Add(31 * day))
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := lf.lim.Sweep(lf.ctx, nil); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			got, err := lf.lim.Limit(lf.ctx, freezeRequest(lf.clock.Now()))
			if err == nil && got.Level != 1 {
				err = fmt.Errorf("limit = %+v, want L1", got)
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := len(lf.pairEvents(EventCapped)); n != 1 {
		t.Fatalf("capped events = %d, want 1", n)
	}
}

// CHECK-20's data: both pages carry the date an L3 pair holds until.
func TestDecayViewsShowHoldsUntil(t *testing.T) {
	lf := newDecayFixture(t)
	lf.clock.Set(decayGenerated.Add(24 * day))
	want := decayGenerated.Add(30 * day)
	v, err := lf.svc.View(lf.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var row, other *ClassView
	for i := range v.Families {
		for j := range v.Families[i].Classes {
			c := &v.Families[i].Classes[j]
			if v.Families[i].Family == FamilyWraparound && c.Class == ClassFreeze {
				row = c
			} else if other == nil {
				other = c
			}
		}
	}
	if row == nil || row.HoldsUntil == nil || !row.HoldsUntil.Equal(want) ||
		!slices.Equal(row.ExpiringChecks, []string{"bench_fresh"}) {
		t.Fatalf("autonomy view row = %+v", row)
	}
	if other == nil || other.HoldsUntil != nil || other.ExpiringChecks != nil {
		t.Fatalf("an L1 pair holds nothing: %+v", other)
	}
	tv, err := lf.svc.TrustView(lf.ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range tv.Rows {
		if r.Family == FamilyWraparound && r.Class == ClassFreeze {
			found = true
			if r.HoldsUntil == nil || !r.HoldsUntil.Equal(want) ||
				!slices.Equal(r.ExpiringChecks, []string{"bench_fresh"}) {
				t.Fatalf("trust row = %+v", r)
			}
		} else if r.HoldsUntil != nil {
			t.Fatalf("%s/%s holds until %v without a level to hold", r.Family, r.Class,
				r.HoldsUntil)
		}
	}
	if !found {
		t.Fatal("no trust row for wraparound/freeze")
	}
}

// An expiry that a slightly newer report moves later is the warning
// already sent; a report that renews it past the lead is warned again
// when its own date comes near.
func TestDecayWarningIsNotRepeatedForAMovingExpiry(t *testing.T) {
	lf := newDecayFixture(t)
	n := &expiryRecorder{}
	lf.clock.Set(decayGenerated.Add(23*day + time.Minute))
	lf.sweep(n)
	ingest := func(generated time.Time) {
		t.Helper()
		if _, err := lf.svc.IngestEvalRun(lf.ctx, benchReport(generated, FamilyWraparound),
			SourceBench, "user:1:admin@example.com", ""); err != nil {
			t.Fatal(err)
		}
	}
	lf.clock.Set(decayGenerated.Add(24*day + time.Minute))
	ingest(decayGenerated.Add(day))
	if res := lf.sweep(n); res.Warned != 0 || n.count() != 1 {
		t.Fatalf("a day later expiry: %+v, %d warnings, want the first only", res, n.count())
	}
	ingest(decayGenerated.Add(9 * day))
	lf.clock.Set(decayGenerated.Add(32*day + time.Minute))
	if res := lf.sweep(n); res.Warned != 1 || n.count() != 2 {
		t.Fatalf("a renewed expiry: %+v, %d warnings, want a second", res, n.count())
	}
	if w := n.warns[1]; !w.HoldsUntil.Equal(decayGenerated.Add(39*day)) ||
		!slices.Equal(w.Checks, []string{"bench_fresh"}) {
		t.Fatalf("second warning = %+v", w)
	}
	if evs := lf.pairEvents(EventEvidenceExpiring); len(evs) != 2 {
		t.Fatalf("expiry events = %d, want 2", len(evs))
	}
}
