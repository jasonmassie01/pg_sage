package earned

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// earned-02: the incident reconciler reads each kind (L2 handoffs, L3
// auto-executions) forward from a cursor in sage.trust_ledger_state and
// re-reads only the ids still waiting (pending: no verdict yet, or the L3
// notice failed). The cursor moves past an id only once its execution is
// older than a 5-minute lag, so a smaller id committing late is still read.

// incidentStateRow is the database's stored incident reconcile state.
type incidentStateRow struct {
	found                       bool
	handoffCursor, autoCursor   *int64
	handoffPending, autoPending []int64
}

func (f *fixture) incidentState() incidentStateRow {
	f.t.Helper()
	var s incidentStateRow
	err := f.pool.QueryRow(f.ctx, `SELECT handoff_cursor, COALESCE(handoff_pending, '{}'),
		auto_cursor, COALESCE(auto_pending, '{}')
		FROM sage.trust_ledger_state WHERE deployment_id = $1 AND database_name = $2`,
		f.store.DeploymentID(), f.db).Scan(&s.handoffCursor, &s.handoffPending,
		&s.autoCursor, &s.autoPending)
	if errors.Is(err, pgx.ErrNoRows) {
		return s
	}
	if err != nil {
		f.t.Fatalf("read incident reconcile state: %v", err)
	}
	s.found = true
	return s
}

// age sets an action's execution time ago before now.
func (f *fixture) age(id int64, ago time.Duration) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_log
		SET executed_at = now() - make_interval(secs => $2::double precision)
		WHERE id = $1`, id, ago.Seconds()); err != nil {
		f.t.Fatalf("age action %d: %v", id, err)
	}
}

// l3Aged is an L3 auto-execution of wraparound_runway x freeze executed
// ago before now.
func (f *fixture) l3Aged(outcome string, ago time.Duration) int64 {
	f.t.Helper()
	id := f.l3Decision("wraparound_runway", "freeze", outcome)
	f.age(id, ago)
	return id
}

// decideVerification completes an action's pending verification.
func (f *fixture) decideVerification(id int64, verdict string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.verification SET verdict = $2,
		completed_at = now() WHERE action_log_id = $1`, id, verdict); err != nil {
		f.t.Fatalf("decide verification of %d: %v", id, err)
	}
}

func (f *fixture) outcomeRows(id int64) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.sre_autonomy_outcomes
		WHERE deployment_id = $1 AND database_name = $2 AND action_log_id = $3`,
		f.store.DeploymentID(), f.db, id).Scan(&n); err != nil {
		f.t.Fatalf("count outcomes of %d: %v", id, err)
	}
	return n
}

func runPass(t *testing.T, ctx context.Context, r *Reconciler, pass int) ReconcileResult {
	t.Helper()
	res, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("pass %d: %v", pass, err)
	}
	return res
}

func requirePending(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("pending = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pending = %v, want %v", got, want)
		}
	}
}

func requireCursor(t *testing.T, what string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s cursor = %v, want %d", what, got, want)
	}
}

// State transition: undecided on pass 1 (pending), still undecided on
// pass 2, decided on pass 3 and recorded exactly once.
func TestReconcileCursorPendingRowIsRecordedOnceDecided(t *testing.T) {
	f := newSelfFixture(t)
	waiting := f.l3Aged("success", 10*time.Minute)
	f.attachVerification(waiting, "pending")
	done := f.l3Aged("success", 8*time.Minute)
	f.attachVerification(done, "success")
	r := NewReconciler(f.svc, f.pool, f.db, nil)

	if res := runPass(t, f.ctx, r, 1); res.Recorded != 1 || res.Pending != 1 {
		t.Fatalf("pass 1 = %+v, want 1 recorded and 1 pending", res)
	}
	st := f.incidentState()
	requireCursor(t, "auto", st.autoCursor, done)
	requirePending(t, st.autoPending, waiting)
	if res := runPass(t, f.ctx, r, 2); res.Recorded != 0 || res.Pending != 1 {
		t.Fatalf("pass 2 = %+v, want the waiting row read again and still pending", res)
	}
	requirePending(t, f.incidentState().autoPending, waiting)

	f.decideVerification(waiting, "success")
	if res := runPass(t, f.ctx, r, 3); res.Recorded != 1 || res.Pending != 0 {
		t.Fatalf("pass 3 = %+v, want the decided row recorded", res)
	}
	st = f.incidentState()
	requireCursor(t, "auto", st.autoCursor, done)
	requirePending(t, st.autoPending)
	if res := runPass(t, f.ctx, r, 4); res.Recorded != 0 || res.Pending != 0 {
		t.Fatalf("pass 4 = %+v, want nothing left", res)
	}
	if got := f.outcomes(f.db)[waiting]; got != "verified_recovery@L3" ||
		f.outcomeRows(waiting) != 1 {
		t.Fatalf("waiting row outcome %q in %d rows, want verified_recovery@L3 once", got,
			f.outcomeRows(waiting))
	}
}

// A failed L3 notice keeps the id pending (retried next pass) while the
// cursor still moves past it.
func TestReconcileCursorRetriesAFailedNoticeAndStillAdvances(t *testing.T) {
	f := newSelfFixture(t)
	id := f.l3Aged("success", 10*time.Minute)
	f.attachVerification(id, "success")
	notifier := &recordingNotifier{fails: true}
	r := NewReconciler(f.svc, f.pool, f.db, notifier)
	if _, err := r.RunOnce(f.ctx); err == nil {
		t.Fatal("a failed notification must surface")
	}
	st := f.incidentState()
	requireCursor(t, "auto", st.autoCursor, id)
	requirePending(t, st.autoPending, id)
	if got := f.outcomes(f.db)[id]; got != "verified_recovery@L3" {
		t.Fatalf("outcome of the unnotified action = %q, want it recorded", got)
	}
	notifier.mu.Lock()
	notifier.fails = false
	notifier.mu.Unlock()
	if res := runPass(t, f.ctx, r, 2); res.Notified != 1 || res.Recorded != 0 {
		t.Fatalf("retry = %+v, want 1 notified and nothing recorded again", res)
	}
	st = f.incidentState()
	requireCursor(t, "auto", st.autoCursor, id)
	requirePending(t, st.autoPending)
	if res := runPass(t, f.ctx, r, 3); res.Notified != 0 || len(notifier.seen) != 2 {
		t.Fatalf("pass 3 = %+v with %d calls, want no third notice", res, len(notifier.seen))
	}
}

// Boundary: a smaller id committed after a larger one, inside the lag, is
// still read; the cursor moves only past rows older than the lag.
func TestReconcileCursorReadsALateCommitInsideTheLag(t *testing.T) {
	f := newSelfFixture(t)
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var late int64
	if err := tx.QueryRow(f.ctx, `WITH d AS (
		INSERT INTO sage.decision (feature, intent, target_objects, verdict, risk_tier,
		  reason, evidence, evidence_id)
		VALUES ('freeze', 'freeze', '["public.orders"]', 'execute', 'safe', 'autonomy_l3',
		  '{"incident_family": "wraparound_runway", "autonomy_class": "freeze"}',
		  md5(random()::text)) RETURNING id)
		INSERT INTO sage.action_log (action_type, sql_executed, outcome, decision_id)
		SELECT 'vacuum_table', 'VACUUM (FREEZE) public.orders', 'failed', id FROM d
		RETURNING id`).Scan(&late); err != nil {
		t.Fatalf("open the late transaction: %v", err)
	}
	early := f.l3Decision("wraparound_runway", "freeze", "failed")
	if late >= early {
		t.Fatalf("ids late %d, early %d: the late commit must hold the smaller id", late,
			early)
	}
	r := NewReconciler(f.svc, f.pool, f.db, nil)
	if res := runPass(t, f.ctx, r, 1); res.Recorded != 1 {
		t.Fatalf("pass 1 = %+v, want the committed row recorded", res)
	}
	if st := f.incidentState(); st.autoCursor != nil {
		t.Fatalf("auto cursor = %d after reading only rows inside the lag, want none",
			*st.autoCursor)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if res := runPass(t, f.ctx, r, 2); res.Recorded != 1 ||
		f.outcomes(f.db)[late] != "not_recovered@L3" {
		t.Fatalf("pass 2 = %+v, outcome of the late commit %q", res, f.outcomes(f.db)[late])
	}
	f.age(early, 10*time.Minute)
	f.age(late, 10*time.Minute)
	runPass(t, f.ctx, r, 3)
	requireCursor(t, "auto", f.incidentState().autoCursor, early)
}

// Restart: a new reconciler (and service) resumes from the stored cursor
// and pending set; it does not rescan below the cursor.
func TestReconcileCursorRestartResumesFromTheStoredState(t *testing.T) {
	f := newSelfFixture(t)
	waiting := f.l3Aged("success", 10*time.Minute)
	f.attachVerification(waiting, "pending")
	var reserved int64
	if err := f.pool.QueryRow(f.ctx, `SELECT nextval(pg_get_serial_sequence(
		'sage.action_log', 'id'))`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	last := f.l3Aged("success", 9*time.Minute)
	f.attachVerification(last, "success")
	runPass(t, f.ctx, NewReconciler(f.svc, f.pool, f.db, nil), 1)
	st := f.incidentState()
	requireCursor(t, "auto", st.autoCursor, last)
	requirePending(t, st.autoPending, waiting)

	// A row below the stored cursor that no pending id names.
	if _, err := f.pool.Exec(f.ctx, `WITH d AS (
		INSERT INTO sage.decision (feature, intent, target_objects, verdict, risk_tier,
		  reason, evidence, evidence_id)
		VALUES ('freeze', 'freeze', '["public.orders"]', 'execute', 'safe', 'autonomy_l3',
		  '{"incident_family": "wraparound_runway", "autonomy_class": "freeze"}',
		  md5(random()::text)) RETURNING id)
		INSERT INTO sage.action_log (id, action_type, sql_executed, outcome, decision_id,
		  executed_at)
		SELECT $1, 'vacuum_table', 'VACUUM (FREEZE) public.orders', 'failed', id,
		       now() - interval '10 minutes' FROM d`, reserved); err != nil {
		t.Fatalf("insert below the cursor: %v", err)
	}
	f.decideVerification(waiting, "success")
	restarted := f.sibling(f.db)
	res := runPass(t, f.ctx, NewReconciler(restarted.svc, f.pool, f.db, nil), 2)
	got := f.outcomes(f.db)
	if res.Recorded != 1 || got[waiting] != "verified_recovery@L3" || got[reserved] != "" {
		t.Fatalf("restarted pass = %+v, outcomes %v: want only the pending row %d recorded",
			res, got, waiting)
	}
	requirePending(t, f.incidentState().autoPending)
}

// Nil/empty: an empty monitored ledger writes no trust_ledger_state row.
func TestReconcileCursorEmptyLedgerWritesNoState(t *testing.T) {
	f := newSelfFixture(t)
	if _, err := f.pool.Exec(f.ctx, "DELETE FROM sage.shadow_decision"); err != nil {
		t.Fatal(err)
	}
	r := NewReconciler(f.svc, f.pool, f.db, nil)
	for pass := 1; pass <= 2; pass++ {
		if res := runPass(t, f.ctx, r, pass); res != (ReconcileResult{}) {
			t.Fatalf("pass %d over an empty ledger = %+v, want nothing", pass, res)
		}
	}
	if st := f.incidentState(); st.found {
		t.Fatalf("an empty ledger wrote its reconcile state: %+v", st)
	}
	var rows int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.trust_ledger_state
		WHERE deployment_id = $1`, f.store.DeploymentID()).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("trust_ledger_state rows = %d (%v), want 0", rows, err)
	}
}

// Concurrent access: two reconcilers of one database passing at once
// record every outcome once and leave one consistent state.
func TestReconcileCursorConcurrentReconcilersRecordOnce(t *testing.T) {
	f := newSelfFixture(t)
	var ids []int64
	for i := 0; i < 20; i++ {
		id := f.l3Aged("failed", 10*time.Minute)
		ids = append(ids, id)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx)
		}(i)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatalf("concurrent passes: %v", err)
	}
	for _, id := range ids {
		if n := f.outcomeRows(id); n != 1 {
			t.Fatalf("action %d has %d outcome rows, want 1", id, n)
		}
	}
	runPass(t, f.ctx, NewReconciler(f.svc, f.pool, f.db, nil), 3)
	st := f.incidentState()
	requireCursor(t, "auto", st.autoCursor, ids[len(ids)-1])
	requirePending(t, st.autoPending)
}

// idlePassRoundTrips pins an idle pass: the incident state, one forward
// read per kind, the self-initiated cursors and their three reads, the
// shadow cursor and its read. Nothing is written.
const idlePassRoundTrips = 9

// newTracedReconFixture is a self fixture whose ledger and monitored pool
// record every statement, with the evidence cache on.
func newTracedReconFixture(t *testing.T) (*fixture, *testdb.QueryRecorder) {
	t.Helper()
	f := newSelfFixture(t)
	rec := &testdb.QueryRecorder{}
	config := f.pool.Config().Copy()
	config.ConnConfig.Tracer = rec
	traced, err := pgxpool.NewWithConfig(f.ctx, config)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	store, err := NewPostgresStore(traced, f.store.DeploymentID(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Now = f.clock.Now
	cfg.EvidenceCacheTTL = time.Hour
	svc, err := NewService(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.pool, f.store, f.svc = traced, store, svc
	return f, rec
}

// Performance: an idle second pass makes no insert, keeps the evidence
// cache and stays within a pinned number of round trips.
func TestReconcileCursorIdlePassDoesNoWork(t *testing.T) {
	f, rec := newTracedReconFixture(t)
	if _, err := f.pool.Exec(f.ctx, "DELETE FROM sage.shadow_decision"); err != nil {
		t.Fatal(err)
	}
	l3 := f.l3Aged("success", 10*time.Minute)
	f.attachVerification(l3, "success")
	handoff := f.actionLog("success")
	f.attachVerification(handoff, "success")
	f.handoff("autonomy:wraparound_runway:freeze:public.orders", handoff)
	f.age(handoff, 10*time.Minute)
	r := NewReconciler(f.svc, f.pool, f.db, &recordingNotifier{})
	if res := runPass(t, f.ctx, r, 1); res.Recorded != 2 || res.Notified != 1 {
		t.Fatalf("pass 1 = %+v, want 2 recorded and 1 notified", res)
	}
	if _, err := f.svc.supportedLevel(f.ctx, FamilyWraparound, ClassFreeze); err != nil {
		t.Fatalf("prime the evidence cache: %v", err)
	}
	rec.Reset()
	f.svc.mu.Lock()
	invalidationsBefore := f.svc.invalidations
	f.svc.mu.Unlock()
	if invalidationsBefore == 0 {
		t.Fatal("pass 1 recorded two outcomes without invalidating the cache")
	}
	if res := runPass(t, f.ctx, r, 2); res != (ReconcileResult{}) {
		t.Fatalf("idle pass = %+v, want nothing", res)
	}
	for _, write := range []string{"INSERT INTO sage.sre_autonomy_outcomes",
		"INSERT INTO sage.sre_autonomy_events", "INSERT INTO sage.trust_ledger_state"} {
		if got := rec.Matching(write); len(got) != 0 {
			t.Fatalf("idle pass sent %d %q statements", len(got), write)
		}
	}
	f.svc.mu.Lock()
	cached, invalidations := len(f.svc.cache), f.svc.invalidations-invalidationsBefore
	f.svc.mu.Unlock()
	if invalidations != 0 {
		t.Fatalf("idle pass invalidated the evidence cache %d times, want 0", invalidations)
	}
	if cached != 1 {
		t.Fatalf("idle pass left %d cached levels, want the primed one (no invalidation)",
			cached)
	}
	if n := rec.RoundTrips(); n > idlePassRoundTrips {
		t.Fatalf("idle pass made %d round trips, want at most %d", n, idlePassRoundTrips)
	}
}

func (f *fixture) invalidationCount() int64 {
	f.svc.mu.Lock()
	defer f.svc.mu.Unlock()
	return f.svc.invalidations
}

// A retry pass that records nothing new keeps the evidence cache; a failed
// outcome write drops it, since the insert may have committed (fail closed).
func TestReconcileCursorRetryPassKeepsTheEvidenceCache(t *testing.T) {
	f := newSelfFixture(t)
	id := f.l3Aged("success", 10*time.Minute)
	f.attachVerification(id, "success")
	r := NewReconciler(f.svc, f.pool, f.db, &recordingNotifier{fails: true})
	if _, err := r.RunOnce(f.ctx); err == nil {
		t.Fatal("a failed notification must surface")
	}
	before := f.invalidationCount()
	if before == 0 {
		t.Fatal("recording a new outcome did not invalidate the evidence cache")
	}
	res, err := r.RunOnce(f.ctx)
	if err == nil || res.Recorded != 0 {
		t.Fatalf("retry pass = %+v (%v), want the notice to fail again and nothing recorded",
			res, err)
	}
	requirePending(t, f.incidentState().autoPending, id)
	if got := f.invalidationCount() - before; got != 0 {
		t.Fatalf("a retry pass that recorded nothing invalidated the cache %d times", got)
	}
	cancelled, cancel := context.WithCancel(f.ctx)
	cancel()
	_, err = f.svc.recordOutcome(cancelled, Outcome{Database: f.db, ActionLogID: id,
		Family: FamilyWraparound, Class: ClassFreeze, Level: L3,
		Result: ResultVerifiedRecovery, Source: SourceExecutor, Actor: ActorPgSage})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("outcome write on a cancelled context: %v, want context.Canceled", err)
	}
	if got := f.invalidationCount() - before; got != 1 {
		t.Fatalf("a failed outcome write invalidated the cache %d times, want 1", got)
	}
}
