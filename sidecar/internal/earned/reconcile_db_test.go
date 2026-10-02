package earned

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Live outcomes feed the promotion evidence: an L2 one-click handoff
// (an approval queue item keyed autonomy:<family>:<class>:<target>) and
// an L3 auto-execution (a decision with reason autonomy_l3) are matched
// to their action_log row and its verification, and recorded once. An
// L3 execution notifies a human once.

type recordingNotifier struct {
	mu    sync.Mutex
	seen  []AutoExecution
	fails bool
}

func (n *recordingNotifier) NotifyAutonomous(_ context.Context, a AutoExecution) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.seen = append(n.seen, a)
	if n.fails {
		return errors.New("webhook down")
	}
	return nil
}

func (f *fixture) actionLog(outcome string) int64 {
	f.t.Helper()
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome) VALUES ('vacuum_table',
		'VACUUM (FREEZE) public.orders', $1) RETURNING id`, outcome).Scan(&id); err != nil {
		f.t.Fatalf("action_log: %v", err)
	}
	return id
}

func (f *fixture) handoff(identity string, actionLogID int64) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO sage.action_queue
		(proposed_sql, action_risk, status, identity_key, action_log_id)
		VALUES ('VACUUM (FREEZE) public.orders', 'safe', 'approved', $1, $2)`,
		identity, actionLogID); err != nil {
		f.t.Fatalf("action_queue: %v", err)
	}
}

func (f *fixture) l3Decision(family, class string, outcome string) int64 {
	f.t.Helper()
	var decision int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.decision
		(feature, intent, target_objects, verdict, risk_tier, reason, evidence, evidence_id)
		VALUES ('freeze', 'freeze', '["public.orders"]', 'execute', 'safe', 'autonomy_l3',
		        jsonb_build_object('incident_family', $1::text, 'autonomy_class', $2::text),
		        md5(random()::text)) RETURNING id`, family, class).Scan(&decision); err != nil {
		f.t.Fatalf("decision: %v", err)
	}
	id := f.actionLog(outcome)
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_log SET decision_id = $1
		WHERE id = $2`, decision, id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) outcomes(database string) map[int64]string {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT action_log_id, result || '@L' || level
		FROM sage.sre_autonomy_outcomes WHERE deployment_id = $1 AND database_name = $2
		  AND action_log_id IS NOT NULL`, f.store.DeploymentID(), database)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			f.t.Fatal(err)
		}
		out[id] = v
	}
	return out
}

func TestReconcileRecordsL2HandoffsAndL3Executions(t *testing.T) {
	f := newFixture(t)
	f.cleanMonitored()
	db := "recon-" + newUUID(t)[:8]
	ok := f.actionLog("success")
	f.handoff("autonomy:wraparound_runway:freeze:public.orders", ok)
	failed := f.actionLog("failed")
	f.handoff("autonomy:wraparound_runway:freeze:public.orders", failed)
	l3 := f.l3Decision("wraparound_runway", "freeze", "success")
	notifier := &recordingNotifier{}
	r := NewReconciler(f.svc, f.pool, db, notifier)
	res, err := r.RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := f.outcomes(db)
	if got[ok] != "verified_recovery@L2" || got[failed] != "not_recovered@L2" ||
		got[l3] != "verified_recovery@L3" || res.Recorded != 3 {
		t.Fatalf("outcomes = %v, result = %+v", got, res)
	}
	if len(notifier.seen) != 1 || notifier.seen[0].ActionLogID != l3 ||
		notifier.seen[0].Family != FamilyWraparound || notifier.seen[0].Class != ClassFreeze ||
		notifier.seen[0].Database != db || res.Notified != 1 {
		t.Fatalf("notifications = %+v", notifier.seen)
	}
	again, err := r.RunOnce(f.ctx)
	if err != nil || again.Recorded != 0 || again.Notified != 0 || len(notifier.seen) != 1 {
		t.Fatalf("second run = %+v (%v); notifications %d", again, err, len(notifier.seen))
	}
	evs := f.events(EventFilter{Family: FamilyWraparound, Class: ClassFreeze, Database: db})
	if len(evs) != 1 || evs[0].Type != EventAutoExecuted || evs[0].ActionLogID != l3 {
		t.Fatalf("auto-executed events = %+v", evs)
	}
}

func TestReconcileWaitsForVerification(t *testing.T) {
	f := newFixture(t)
	f.cleanMonitored()
	db := "recon-" + newUUID(t)[:8]
	id := f.l3Decision("wraparound_runway", "freeze", "success")
	var decision int64
	if err := f.pool.QueryRow(f.ctx, `SELECT decision_id FROM sage.action_log WHERE id = $1`,
		id).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	var verification int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.verification
		(decision_id, action_log_id, criterion, baseline, minimum_samples,
		 next_evaluation_at, hard_deadline_at, verdict)
		VALUES ($1, $2, '{}', '{}', 3, now(), now() + interval '1 hour', 'pending')
		RETURNING id`, decision, id).Scan(&verification); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.action_log SET verification_id = $1
		WHERE id = $2`, verification, id); err != nil {
		t.Fatal(err)
	}
	notifier := &recordingNotifier{}
	r := NewReconciler(f.svc, f.pool, db, notifier)
	res, err := r.RunOnce(f.ctx)
	if err != nil || res.Pending != 1 || len(f.outcomes(db)) != 0 {
		t.Fatalf("pending verification = %+v (%v), outcomes %v", res, err, f.outcomes(db))
	}
	if len(notifier.seen) != 1 {
		t.Fatal("the human is notified at execution, not after verification")
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE sage.verification SET verdict = 'revert'
		WHERE id = $1`, verification); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.outcomes(db)[id]; got != "harmful@L3" {
		t.Fatalf("reverted action outcome = %q", got)
	}
}

// A rolled-back action is harmful: the family is demoted at once.
func TestReconcileRollbackDemotesTheFamily(t *testing.T) {
	f := newFixture(t)
	f.cleanMonitored()
	f.seedL3()
	db := "recon-" + newUUID(t)[:8]
	f.handoff("autonomy:wraparound_runway:freeze:public.orders", f.actionLog("rolled_back"))
	if _, err := NewReconciler(f.svc, f.pool, db, nil).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.granted(FamilyWraparound, ClassFreeze); got != L1 {
		t.Fatalf("level after a rolled-back action = %v", got)
	}
}

func TestReconcileSkipsMalformedKeysAndKeepsGoing(t *testing.T) {
	f := newFixture(t)
	f.cleanMonitored()
	db := "recon-" + newUUID(t)[:8]
	for _, key := range []string{"autonomy:bogus", "autonomy:shell:freeze:public.t",
		"autonomy:wraparound_runway:rm_rf:public.t", "autonomy:::"} {
		f.handoff(key, f.actionLog("success"))
	}
	good := f.actionLog("success")
	f.handoff("autonomy:wal_retention:wal_bound:slot:s1", good)
	res, err := NewReconciler(f.svc, f.pool, db, nil).RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.outcomes(db); len(got) != 1 || got[good] != "verified_recovery@L2" ||
		res.Skipped < 4 {
		t.Fatalf("outcomes = %v, result = %+v", got, res)
	}
}

func TestReconcileNotifierFailureIsRetried(t *testing.T) {
	f := newFixture(t)
	f.cleanMonitored()
	db := "recon-" + newUUID(t)[:8]
	f.l3Decision("wraparound_runway", "freeze", "success")
	notifier := &recordingNotifier{fails: true}
	r := NewReconciler(f.svc, f.pool, db, notifier)
	if _, err := r.RunOnce(f.ctx); err == nil {
		t.Fatal("a failed notification must surface")
	}
	notifier.fails = false
	res, err := r.RunOnce(f.ctx)
	if err != nil || res.Notified != 1 || len(notifier.seen) != 2 {
		t.Fatalf("retry = %+v (%v), calls %d", res, err, len(notifier.seen))
	}
}

func TestReconcilerValidatesInputs(t *testing.T) {
	f := newFixture(t)
	if _, err := NewReconciler(f.svc, nil, "db", nil).RunOnce(f.ctx); err == nil {
		t.Fatal("nil monitored pool accepted")
	}
	if _, err := NewReconciler(f.svc, f.pool, "", nil).RunOnce(f.ctx); err == nil {
		t.Fatal("empty database accepted")
	}
}

func TestPostgresConcurrencyCountsOtherWriters(t *testing.T) {
	f := newFixture(t)
	target := "public.conc_" + newUUID(t)[:8]
	c := NewPostgresConcurrency(f.pool, nil)
	n, err := c.ConcurrentActions(f.ctx, []string{target}, false, 15*time.Minute)
	if err != nil || n != 0 {
		t.Fatalf("idle object = %d (%v)", n, err)
	}
	var decision int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.decision (feature, intent,
		target_objects, verdict, risk_tier, reason, evidence_id)
		VALUES ('index', 'index', jsonb_build_array($1::text), 'execute', 'moderate',
		        'authorized', md5(random()::text)) RETURNING id`, target).
		Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO sage.change_lease (object_key,
		decision_id, holder, intent, expires_at) VALUES ($1, $2, 'executor', 'index',
		now() + interval '5 minutes')`, target, decision); err != nil {
		t.Fatal(err)
	}
	if n, err = c.ConcurrentActions(f.ctx, []string{target}, false, 15*time.Minute); err != nil ||
		n != 1 {
		t.Fatalf("active lease = %d (%v)", n, err)
	}
	if n, err = c.ConcurrentActions(f.ctx, []string{target}, true, 15*time.Minute); err != nil ||
		n != 0 {
		t.Fatalf("our own lease = %d (%v)", n, err)
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO sage.action_log (action_type,
		sql_executed, outcome, decision_id, executed_at) VALUES ('create_index_concurrently',
		'CREATE INDEX CONCURRENTLY x', 'success', $1, now() - interval '10 minutes')`,
		decision); err != nil {
		t.Fatal(err)
	}
	if n, err = c.ConcurrentActions(f.ctx, []string{target}, true, 15*time.Minute); err != nil ||
		n != 1 {
		t.Fatalf("recent action on the object = %d (%v)", n, err)
	}
	if n, err = c.ConcurrentActions(f.ctx, []string{target}, true, 5*time.Minute); err != nil ||
		n != 0 {
		t.Fatalf("action older than the window = %d (%v)", n, err)
	}
	if n, err = c.ConcurrentActions(f.ctx, []string{"public.other_" + newUUID(t)[:8]}, false,
		15*time.Minute); err != nil || n != 0 {
		t.Fatalf("other object = %d (%v)", n, err)
	}
	if _, err := c.ConcurrentActions(f.ctx, nil, false, time.Minute); err == nil {
		t.Fatal("no targets must be an error (fail closed)")
	}
}

// cleanMonitored empties the monitored-database tables the reconciler
// scans. The package fixture database is private to this package and
// its tests do not run in parallel.
func (f *fixture) cleanMonitored() {
	f.t.Helper()
	for _, stmt := range []string{
		"DELETE FROM sage.action_queue",
		"UPDATE sage.action_log SET decision_id = NULL, verification_id = NULL",
		"DELETE FROM sage.verification",
		"DELETE FROM sage.change_lease",
		"DELETE FROM sage.action_log",
		"DELETE FROM sage.decision",
	} {
		if _, err := f.pool.Exec(f.ctx, stmt); err != nil {
			f.t.Fatalf("%s: %v", stmt, err)
		}
	}
}
