package earned

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// earned-01: an operator's rollback of an incident-family action is a
// one-level pair demerit, like a rejection, and the rolled-back action
// never counts toward the verified L2 recoveries L3 needs, whether the
// rollback landed before or after the reconciler first read the action.

const freezeHandoff = "autonomy:wraparound_runway:freeze:public.orders"

// rollBack marks an action rolled back by an operator ago before now.
func (f *fixture) rollBack(actionLogID int64, ago time.Duration) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_log
		SET outcome = 'rolled_back', rollback_reason = 'manual rollback',
		    measured_at = now() - make_interval(secs => $2)
		WHERE id = $1`, actionLogID, ago.Seconds()); err != nil {
		f.t.Fatalf("roll back action %d: %v", actionLogID, err)
	}
}

// improvedHandoff is an executed L2 freeze handoff with verdict improved.
func (f *fixture) improvedHandoff() int64 {
	f.t.Helper()
	id := f.actionLog("success")
	f.attachOutcome(id, "improved")
	f.handoff(freezeHandoff, id)
	return id
}

func (f *fixture) reconcileWith(n Notifier) ReconcileResult {
	f.t.Helper()
	res, err := NewReconciler(f.svc, f.pool, f.db, n).RunOnce(f.ctx)
	if err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (f *fixture) verifiedL2(family Family, class ActionClass) int {
	f.t.Helper()
	ev, err := f.svc.Evidence(f.ctx, family, class)
	if err != nil {
		f.t.Fatalf("evidence %s/%s: %v", family, class, err)
	}
	return ev.Live.VerifiedL2
}

func (f *fixture) downgrades(family Family, class ActionClass) []Event {
	f.t.Helper()
	var out []Event
	for _, e := range f.events(EventFilter{Family: family, Class: class, Database: f.db}) {
		if e.Type == EventAutoDowngraded {
			out = append(out, e)
		}
	}
	return out
}

// rollbackState is what a rollback leaves behind for the freeze pair.
type rollbackState struct {
	level      Level
	verifiedL2 int
	harmful    int
	rows       []selfOutcomeRow
	downgrades int
}

// incidentRollback runs one L2 freeze handoff that improved and was then
// rolled back by an operator, either after the reconciler credited it or
// before the reconciler first read it.
func incidentRollback(t *testing.T, rollBackFirst bool) (rollbackState, int64) {
	f := newSelfFixture(t)
	f.seedL3()
	if got := f.verifiedL2(FamilyWraparound, ClassFreeze); got != 50 {
		t.Fatalf("seeded verified L2 = %d, want 50", got)
	}
	id := f.improvedHandoff()
	if rollBackFirst {
		f.rollBack(id, 0)
	} else {
		f.reconcile()
		if got := f.verifiedL2(FamilyWraparound, ClassFreeze); got != 51 {
			t.Fatalf("credited handoff: verified L2 = %d, want 51", got)
		}
		if lv := f.granted(FamilyWraparound, ClassFreeze); lv != L3 {
			t.Fatalf("a credited handoff changed the level: %v", lv)
		}
		f.rollBack(id, 0)
	}
	n := &demotionNotifier{}
	res := f.reconcileWith(n)
	if res.Demoted != 1 || len(n.demotions) != 1 || n.demotions[0].Cause != CauseRolledBack ||
		n.demotions[0].ActionLogID != id || n.demotions[0].From != L3 ||
		n.demotions[0].To != L2 {
		t.Fatalf("rollback pass = %+v, demotions %+v", res, n.demotions)
	}
	ev, err := f.svc.Evidence(f.ctx, FamilyWraparound, ClassFreeze)
	if err != nil {
		t.Fatal(err)
	}
	return rollbackState{level: f.granted(FamilyWraparound, ClassFreeze),
		verifiedL2: ev.Live.VerifiedL2, harmful: ev.Live.HarmfulPair,
		rows: f.selfOutcomes()[id], downgrades: len(f.downgrades(FamilyWraparound,
			ClassFreeze))}, id
}

func TestIncidentRollbackAfterCreditDemotesOneLevelAndDropsTheCredit(t *testing.T) {
	st, _ := incidentRollback(t, false)
	if st.level != L2 || st.verifiedL2 != 50 || st.harmful != 0 || st.downgrades != 1 {
		t.Fatalf("after the rollback: %+v", st)
	}
	want := []selfOutcomeRow{
		{"wraparound_runway", "freeze", "", ResultVerifiedRecovery, SourceExecutor, 2},
		{"wraparound_runway", "freeze", CauseRolledBack, ResultRejected, SourceRollback, 2},
	}
	if len(st.rows) != 2 || st.rows[0] != want[0] || st.rows[1] != want[1] {
		t.Fatalf("outcome rows = %+v, want %+v", st.rows, want)
	}
}

func TestIncidentRollbackBeforeTheFirstPassReachesTheSameState(t *testing.T) {
	after, _ := incidentRollback(t, false)
	before, _ := incidentRollback(t, true)
	if before.level != after.level || before.verifiedL2 != after.verifiedL2 ||
		before.harmful != after.harmful || before.downgrades != after.downgrades {
		t.Fatalf("rollback before the first pass = %+v, after = %+v", before, after)
	}
	if len(before.rows) != 2 || before.rows[0].result != ResultUnverified ||
		before.rows[1].source != SourceRollback || before.rows[1].result != ResultRejected {
		t.Fatalf("outcome rows = %+v", before.rows)
	}
}

// A carried-over level is granted by policy and is not demoted
// automatically (M7), but the rolled-back action stops counting.
func TestIncidentRollbackCarriedOverPairLosesTheCreditKeepsItsLevel(t *testing.T) {
	f := newSelfFixture(t)
	if _, err := f.svc.SeedCarriedOver(f.ctx, f.db, autonomousBound()); err != nil {
		t.Fatal(err)
	}
	id := f.improvedHandoff()
	f.reconcile()
	if got := f.verifiedL2(FamilyWraparound, ClassFreeze); got != 1 {
		t.Fatalf("credited handoff: verified L2 = %d, want 1", got)
	}
	f.rollBack(id, 0)
	res := f.reconcile()
	st := f.state(FamilyWraparound, ClassFreeze)
	if res.Demoted != 0 || st.Level != L3 || st.Provenance != ProvenanceCarriedOver {
		t.Fatalf("carried-over pair after a rollback: %+v (result %+v)", st, res)
	}
	if got := f.verifiedL2(FamilyWraparound, ClassFreeze); got != 0 {
		t.Fatalf("rolled-back handoff still counts: verified L2 = %d", got)
	}
	if rows := f.selfOutcomes()[id]; len(rows) != 2 || rows[1].source != SourceRollback {
		t.Fatalf("outcome rows = %+v", rows)
	}
	if d := f.downgrades(FamilyWraparound, ClassFreeze); len(d) != 0 {
		t.Fatalf("carried-over pair demoted: %+v", d)
	}
}

// An L3 auto-execution is judged under its decision's pair at L3.
func TestIncidentRollbackOfAnL3ExecutionDemotesThePair(t *testing.T) {
	f := newSelfFixture(t)
	f.seedL3()
	id := f.l3Decision("wraparound_runway", "freeze", "success")
	f.attachOutcome(id, "improved")
	f.reconcile()
	f.rollBack(id, 0)
	res := f.reconcile()
	rows := f.selfOutcomes()[id]
	if res.Demoted != 1 || len(rows) != 2 || rows[0].result != ResultVerifiedRecovery ||
		rows[1] != (selfOutcomeRow{"wraparound_runway", "freeze", CauseRolledBack,
			ResultRejected, SourceRollback, 3}) {
		t.Fatalf("L3 rollback = %+v, rows %+v", res, rows)
	}
	if got := f.granted(FamilyWraparound, ClassFreeze); got != L2 {
		t.Fatalf("freeze after an operator rollback = %v, want L2", got)
	}
	// Other classes of the family are untouched: one pair, one level.
	if got := f.granted(FamilyWraparound, ClassVacuum); got != defaultLevel(FamilyWraparound) {
		t.Fatalf("vacuum moved with freeze: %v", got)
	}
}

// The rollback of an incident action's own regression is not a second
// demerit: the regression already demoted the whole family (M7).
func TestIncidentRollbackOfARegressionKeepsTheFamilyRule(t *testing.T) {
	f := newSelfFixture(t)
	f.seedL3()
	id := f.actionLog("success")
	f.attachOutcome(id, "regressed")
	f.handoff(freezeHandoff, id)
	f.rollBack(id, 0)
	res := f.reconcile()
	rows := f.selfOutcomes()[id]
	if len(rows) != 1 || rows[0].result != ResultHarmful || rows[0].source != SourceExecutor {
		t.Fatalf("regression rows = %+v", rows)
	}
	if res.Demoted != 0 || f.granted(FamilyWraparound, ClassFreeze) != L1 {
		t.Fatalf("regressed then rolled back = %+v, level %v", res,
			f.granted(FamilyWraparound, ClassFreeze))
	}
}

// The no-gain revert of a neutral index create is no demerit in an
// incident family either.
func TestIncidentRollbackNeutralIndexCreateRevertIsNoDemerit(t *testing.T) {
	f := newSelfFixture(t)
	id := f.actionLog("success")
	f.attachOutcome(id, "neutral")
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_outcome SET action_class =
		'index_create' WHERE action_log_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	f.handoff("autonomy:plan_regression:index_create:public.orders", id)
	f.rollBack(id, 0)
	res := f.reconcile()
	rows := f.selfOutcomes()[id]
	if res.Demoted != 0 || len(rows) != 1 || rows[0] != (selfOutcomeRow{"plan_regression",
		"index_create", "", ResultUnverified, SourceExecutor, 2}) {
		t.Fatalf("no-gain revert = %+v, rows %+v", res, rows)
	}
}

// A malformed incident marker is skipped, never judged as a self class,
// and does not stop the pass.
func TestIncidentRollbackSkipsAMalformedIncidentMarker(t *testing.T) {
	f := newSelfFixture(t)
	bad := f.l3Decision("shell", "freeze", "success")
	f.rollBack(bad, time.Minute)
	good := f.improvedHandoff()
	f.rollBack(good, 0)
	f.reconcile()
	got := f.selfOutcomes()
	for _, r := range got[bad] {
		if r.source == SourceRollback {
			t.Fatalf("malformed incident action judged: %+v", r)
		}
	}
	if rows := got[good]; len(rows) != 2 || rows[1].source != SourceRollback {
		t.Fatalf("well-formed rollback after a malformed one = %+v", rows)
	}
}

// Two reconcilers (two sidecars) reading one rollback demote once.
func TestIncidentRollbackConcurrentReconcilersDemoteOnce(t *testing.T) {
	f := newSelfFixture(t)
	f.seedL3()
	id := f.improvedHandoff()
	f.reconcile()
	f.rollBack(id, 0)
	other, err := NewService(f.store, f.svc.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]ReconcileResult, 2)
	errs := make([]error, 2)
	for i, svc := range []*Service{f.svc, other} {
		wg.Add(1)
		go func(i int, svc *Service) {
			defer wg.Done()
			<-start
			results[i], errs[i] = NewReconciler(svc, f.pool, f.db, nil).RunOnce(f.ctx)
		}(i, svc)
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent passes: %v, %v", errs[0], errs[1])
	}
	if d := results[0].Demoted + results[1].Demoted; d != 1 {
		t.Fatalf("demotions = %d (%+v), want 1", d, results)
	}
	if d := f.downgrades(FamilyWraparound, ClassFreeze); len(d) != 1 ||
		!strings.Contains(d[0].Reason, "rolled back by an operator") {
		t.Fatalf("auto_downgraded events = %+v, want 1", d)
	}
	if rows := f.selfOutcomes()[id]; len(rows) != 2 {
		t.Fatalf("outcome rows = %+v", rows)
	}
	if got := f.granted(FamilyWraparound, ClassFreeze); got != L2 {
		t.Fatalf("freeze = %v, want L2", got)
	}
}

// failRollbackInserts makes every rollback outcome insert fail until the
// returned func drops the trigger.
func (f *fixture) failRollbackInserts() func() {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `CREATE OR REPLACE FUNCTION
		public.earned01_fail_rollback() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.source = 'rollback' THEN
				RAISE EXCEPTION 'injected rollback outcome failure';
			END IF;
			RETURN NEW;
		END $$;
		DROP TRIGGER IF EXISTS earned01_fail_rollback ON sage.sre_autonomy_outcomes;
		CREATE TRIGGER earned01_fail_rollback BEFORE INSERT ON sage.sre_autonomy_outcomes
			FOR EACH ROW EXECUTE FUNCTION public.earned01_fail_rollback()`); err != nil {
		f.t.Fatalf("install failing trigger: %v", err)
	}
	drop := func() {
		if _, err := f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS
			earned01_fail_rollback ON sage.sre_autonomy_outcomes;
			DROP FUNCTION IF EXISTS public.earned01_fail_rollback()`); err != nil {
			f.t.Errorf("drop failing trigger: %v", err)
		}
	}
	f.t.Cleanup(drop)
	return drop
}

func (f *fixture) rollbackCursor() *time.Time {
	f.t.Helper()
	c, err := f.store.cursors(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return c.rollback
}

// A failed rollback record is returned and leaves the cursor where it
// was, so the next pass judges the rollback.
func TestIncidentRollbackRecordFailureKeepsTheCursor(t *testing.T) {
	f := newSelfFixture(t)
	f.seedL3()
	first := f.improvedHandoff()
	f.rollBack(first, 2*time.Minute)
	f.reconcile()
	cursor := f.rollbackCursor()
	if cursor == nil || f.granted(FamilyWraparound, ClassFreeze) != L2 {
		t.Fatalf("first rollback: cursor %v, level %v", cursor,
			f.granted(FamilyWraparound, ClassFreeze))
	}
	second := f.improvedHandoff()
	f.rollBack(second, time.Minute)
	drop := f.failRollbackInserts()
	_, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx)
	if err == nil || !strings.Contains(err.Error(), "injected rollback outcome failure") ||
		!strings.Contains(err.Error(), "record autonomy outcome") {
		t.Fatalf("failed rollback record = %v, want it returned with context", err)
	}
	if got := f.rollbackCursor(); got == nil || !got.Equal(*cursor) {
		t.Fatalf("cursor moved on a failed record: %v, was %v", got, cursor)
	}
	if got := f.granted(FamilyWraparound, ClassFreeze); got != L2 {
		t.Fatalf("level moved on a failed record: %v", got)
	}
	drop()
	res := f.reconcile()
	if res.Demoted != 1 || f.granted(FamilyWraparound, ClassFreeze) != L1 {
		t.Fatalf("retry pass = %+v, level %v", res, f.granted(FamilyWraparound, ClassFreeze))
	}
	if got := f.rollbackCursor(); got == nil || !got.After(*cursor) {
		t.Fatalf("cursor after the retry = %v, want after %v", got, cursor)
	}
}

// A rollback recorded for another database of the deployment is not this
// database's: its own verified recovery of the same action id counts.
func TestIncidentRollbackOfAnotherDatabaseKeepsTheCredit(t *testing.T) {
	f := newSelfFixture(t)
	id := f.improvedHandoff()
	f.reconcile()
	other := f.sibling("other-" + newUUID(t)[:8])
	if err := other.svc.RecordOutcome(other.ctx, Outcome{Database: other.db,
		ActionLogID: id, Family: FamilyWraparound, Class: ClassFreeze, Level: L2,
		Result: ResultRejected, Source: SourceRollback, Verdict: CauseRolledBack,
		Actor: ActorPgSage}); err != nil {
		t.Fatal(err)
	}
	if got := f.verifiedL2(FamilyWraparound, ClassFreeze); got != 1 {
		t.Fatalf("verified L2 = %d, want 1 (the rollback is another database's)", got)
	}
	if got := other.verifiedL2(FamilyWraparound, ClassFreeze); got != 0 {
		t.Fatalf("other database verified L2 = %d, want 0", got)
	}
}
