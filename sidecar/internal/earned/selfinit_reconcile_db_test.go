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

// The reconciler turns self-initiated actions' verdicts
// (sage.action_outcome), operator rollbacks and operator rejections into
// trust evidence for (database, family, class). A demerit (regressed,
// rolled back by an operator, rejected) demotes the pair one level, is
// recorded with its cause, and notifies an operator. A demerit already
// known when the level was set does not demote it again.

type demotionNotifier struct {
	mu        sync.Mutex
	demotions []Demotion
	autos     []AutoExecution
	fail      bool
}

func (n *demotionNotifier) NotifyAutonomous(_ context.Context, a AutoExecution) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.autos = append(n.autos, a)
	return nil
}

func (n *demotionNotifier) NotifyDemotion(_ context.Context, d Demotion) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.demotions = append(n.demotions, d)
	if n.fail {
		return errors.New("chat webhook down")
	}
	return nil
}

// selfAction records one executed self-initiated action in the monitored
// database: its decision (reason), action_log row (lifecycle outcome) and,
// with a verdict, its decided sage.action_outcome row.
func (f *fixture) selfAction(actionType, sql, outcomeClass, lifecycle, verdict,
	reason string, decidedAgo time.Duration) int64 {
	f.t.Helper()
	var decision, id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.decision
		(feature, intent, target_objects, verdict, risk_tier, reason, evidence, evidence_id)
		VALUES ('index', 'index', '["public.orders"]', 'execute', 'moderate', $1, '{}',
		        md5(random()::text)) RETURNING id`, reason).Scan(&decision); err != nil {
		f.t.Fatalf("decision: %v", err)
	}
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, decision_id, executed_at, measured_at)
		VALUES ($1, $2, $3, $4, now() - interval '1 day',
		        CASE WHEN $3 = 'rolled_back' THEN now() END) RETURNING id`,
		actionType, sql, lifecycle, decision).Scan(&id); err != nil {
		f.t.Fatalf("action_log: %v", err)
	}
	if verdict == "" {
		return id
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO sage.action_outcome
		(action_log_id, action_class, predicted, prediction_method, verdict, tolerance,
		 window_start, window_end, decided_at)
		VALUES ($1, $2, '{}', 'rule', $3, 'unmeasured', now() - interval '1 day',
		        now() - make_interval(secs => $4), now() - make_interval(secs => $4))`,
		id, outcomeClass, verdict, decidedAgo.Seconds()); err != nil {
		f.t.Fatalf("action_outcome: %v", err)
	}
	return id
}

// rejectedProposal is an approval queue item an operator rejected.
func (f *fixture) rejectedProposal(actionType, sql, identity string) int64 {
	f.t.Helper()
	var id int64
	if err := f.pool.QueryRow(f.ctx, `INSERT INTO sage.action_queue
		(proposed_sql, action_risk, status, decided_by, decided_at, action_type,
		 identity_key, reason)
		VALUES ($1, 'moderate', 'rejected', 7, now(), $2, NULLIF($3, ''), 'not now')
		RETURNING id`, sql, actionType, identity).Scan(&id); err != nil {
		f.t.Fatalf("action_queue: %v", err)
	}
	return id
}

type selfOutcomeRow struct {
	family, class, verdict, result, source string
	level                                  int
}

func (f *fixture) selfOutcomes() map[int64][]selfOutcomeRow {
	f.t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT COALESCE(action_log_id, -queue_id), family,
		action_class, COALESCE(verdict, ''), result, source, level
		FROM sage.sre_autonomy_outcomes WHERE deployment_id = $1 AND database_name = $2
		ORDER BY id`, f.store.DeploymentID(), f.db)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64][]selfOutcomeRow{}
	for rows.Next() {
		var id int64
		var r selfOutcomeRow
		if err := rows.Scan(&id, &r.family, &r.class, &r.verdict, &r.result, &r.source,
			&r.level); err != nil {
			f.t.Fatal(err)
		}
		out[id] = append(out[id], r)
	}
	return out
}

// newSelfFixture is a reconciler fixture whose clock follows the
// database's, so verdict times and level changes compare.
func newSelfFixture(t *testing.T) *fixture {
	t.Helper()
	f := newReconFixture(t)
	f.cleanMonitored()
	f.clock.Set(time.Now().UTC().Add(-time.Hour))
	return f
}

func TestSelfReconcileRecordsVerdictsAsEvidence(t *testing.T) {
	f := newSelfFixture(t)
	improved := f.selfAction("create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i1 ON public.orders (a)", "index_create", "success",
		"improved", "autonomy_l3", time.Minute)
	noGain := f.selfAction("create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i2 ON public.orders (b)", "index_create", "rolled_back",
		"neutral", "autonomy_l3", time.Minute)
	vacuum := f.selfAction("vacuum_table", "VACUUM public.orders", "vacuum", "success",
		"neutral", "operator_approved", time.Minute)
	analyze := f.selfAction("analyze_table", "ANALYZE public.orders", "analyze",
		"unverifiable", "insufficient_evidence", "authorized", time.Minute)
	guc := f.selfAction("alter_system_guc", "ALTER SYSTEM SET work_mem = '64MB'", "guc",
		"rolled_back", "regressed", "autonomy_l3", time.Minute)
	res, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := f.selfOutcomes()
	want := map[int64]selfOutcomeRow{
		improved: {"tuning", "index_create", "improved", ResultVerifiedRecovery, "executor", 3},
		noGain:   {"tuning", "index_create", "neutral", ResultUnverified, "executor", 3},
		vacuum:   {"hygiene", "vacuum", "neutral", ResultVerifiedRecovery, "executor", 1},
		analyze:  {"hygiene", "analyze", "insufficient_evidence", ResultUnverified, "executor", 1},
		guc:      {"tuning", "config_guc", "regressed", ResultHarmful, "executor", 3},
	}
	for id, w := range want {
		if len(got[id]) != 1 || got[id][0] != w {
			t.Errorf("action %d: %+v, want [%+v]", id, got[id], w)
		}
	}
	if res.SelfRecorded != 5 {
		t.Fatalf("self recorded = %d, want 5 (%+v)", res.SelfRecorded, res)
	}
	again, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx)
	if err != nil || again.SelfRecorded != 0 || len(f.selfOutcomes()) != 5 {
		t.Fatalf("second pass = %+v (%v), outcomes %v", again, err, f.selfOutcomes())
	}
}

func (f *fixture) grandfatherAt(level Level) {
	f.t.Helper()
	bound := autonomousBound(f.clock.Now(), 90*24*time.Hour)
	if level == L2 {
		bound.TrustLevel = policy.TrustAdvisory
	}
	if _, err := f.svc.SeedGrandfathered(f.ctx, f.db, bound); err != nil {
		f.t.Fatalf("grandfather: %v", err)
	}
}

func TestSelfReconcileRegressionDemotesOneLevelAndNotifies(t *testing.T) {
	f := newSelfFixture(t)
	f.grandfatherAt(L3)
	drop := f.selfAction("drop_unused_index", "DROP INDEX CONCURRENTLY public.i_old",
		"index_drop", "rolled_back", "regressed", "autonomy_l3", 0)
	n := &demotionNotifier{}
	res, err := NewReconciler(f.svc, f.pool, f.db, n).RunOnce(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	st := f.state(FamilyHygiene, ClassIndexDrop)
	if st.Level != L2 || st.Provenance != ProvenanceLedger || res.Demoted != 1 {
		t.Fatalf("after a regression: %+v (result %+v)", st, res)
	}
	if !strings.Contains(st.Reason, "regressed") {
		t.Fatalf("level reason %q does not name the cause", st.Reason)
	}
	evs := f.events(EventFilter{Family: FamilyHygiene, Class: ClassIndexDrop, Database: f.db})
	if len(evs) == 0 || evs[0].Type != EventAutoDowngraded || evs[0].ActionLogID != drop ||
		*evs[0].From != L3 || *evs[0].To != L2 || !strings.Contains(evs[0].Reason, "regressed") {
		t.Fatalf("demotion event = %+v", evs)
	}
	if len(n.demotions) != 1 || n.demotions[0].From != L3 || n.demotions[0].To != L2 ||
		n.demotions[0].Cause != CauseRegressed || n.demotions[0].ActionLogID != drop ||
		n.demotions[0].Database != f.db || n.demotions[0].Class != ClassIndexDrop {
		t.Fatalf("notifications = %+v", n.demotions)
	}
	// Other classes of the family are untouched: one class, one level.
	if got := f.state(FamilyHygiene, ClassVacuum).Level; got != L3 {
		t.Fatalf("vacuum demoted with index_drop: %v", got)
	}
	again, err := NewReconciler(f.svc, f.pool, f.db, n).RunOnce(f.ctx)
	if err != nil || again.Demoted != 0 || len(n.demotions) != 1 ||
		f.state(FamilyHygiene, ClassIndexDrop).Level != L2 {
		t.Fatalf("second pass demoted again: %+v (%v)", again, err)
	}
}

func TestSelfReconcileRejectionDemotes(t *testing.T) {
	f := newSelfFixture(t)
	f.grandfatherAt(L2)
	queueID := f.rejectedProposal("create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i3 ON public.orders (c)", "")
	n := &demotionNotifier{}
	if _, err := NewReconciler(f.svc, f.pool, f.db, n).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if st := f.state(FamilyTuning, ClassIndexCreate); st.Level != L1 {
		t.Fatalf("after a rejection: %+v", st)
	}
	rows := f.selfOutcomes()[-queueID]
	if len(rows) != 1 || rows[0].result != ResultRejected || rows[0].verdict != "rejected" ||
		rows[0].source != SourceOperator || rows[0].level != 2 {
		t.Fatalf("rejection evidence = %+v", rows)
	}
	if len(n.demotions) != 1 || n.demotions[0].Cause != CauseRejected ||
		n.demotions[0].QueueID != queueID {
		t.Fatalf("notifications = %+v", n.demotions)
	}
}

// An incident-family handoff an operator rejects demotes that pair one
// level too: one trust system.
func TestSelfReconcileRejectedHandoffDemotesTheIncidentPair(t *testing.T) {
	f := newSelfFixture(t)
	f.seedL3()
	f.rejectedProposal("vacuum_table", "VACUUM (FREEZE) public.orders",
		"autonomy:wraparound_runway:freeze:public.orders")
	if _, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.granted(FamilyWraparound, ClassFreeze); got != L2 {
		t.Fatalf("freeze after a rejected handoff = %v, want L2", got)
	}
}

func TestSelfReconcileOperatorRollbackDemotes(t *testing.T) {
	f := newSelfFixture(t)
	f.grandfatherAt(L3)
	id := f.selfAction("alter_system_guc", "ALTER SYSTEM SET work_mem = '64MB'", "guc",
		"rolled_back", "improved", "autonomy_l3", 10*time.Minute)
	if _, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	rows := f.selfOutcomes()[id]
	if len(rows) != 2 || rows[0].result != ResultVerifiedRecovery ||
		rows[1].result != ResultRejected || rows[1].verdict != "rolled_back" ||
		rows[1].source != SourceRollback {
		t.Fatalf("improved then rolled back = %+v", rows)
	}
	if st := f.state(FamilyTuning, ClassConfigGUC); st.Level != L2 ||
		!strings.Contains(st.Reason, "rolled back") {
		t.Fatalf("after an operator rollback: %+v", st)
	}
}

// The automatic revert of a neutral index create is the Phase 1.3 no-gain
// rule, not a demerit.
func TestSelfReconcileNoGainRevertIsNotADemerit(t *testing.T) {
	f := newSelfFixture(t)
	f.grandfatherAt(L3)
	f.selfAction("create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i4 ON public.orders (d)", "index_create", "rolled_back",
		"neutral", "autonomy_l3", 0)
	res, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx)
	if err != nil || res.Demoted != 0 {
		t.Fatalf("no-gain revert = %+v (%v)", res, err)
	}
	if got := f.state(FamilyTuning, ClassIndexCreate).Level; got != L3 {
		t.Fatalf("index_create = %v after a no-gain revert, want L3", got)
	}
}

// A regression decided before the level was set was already known: it is
// evidence (it resets the success streak) but does not demote again.
func TestSelfReconcileOldDemeritDoesNotDemote(t *testing.T) {
	f := newSelfFixture(t)
	f.selfAction("drop_unused_index", "DROP INDEX CONCURRENTLY public.i5", "index_drop",
		"rolled_back", "regressed", "autonomy_l3", 3*time.Hour)
	f.clock.Set(time.Now().UTC())
	f.grandfatherAt(L3)
	res, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx)
	if err != nil || res.Demoted != 0 || res.SelfRecorded != 1 {
		t.Fatalf("old regression = %+v (%v)", res, err)
	}
	if got := f.state(FamilyHygiene, ClassIndexDrop).Level; got != L3 {
		t.Fatalf("index_drop = %v, want L3", got)
	}
	ev, err := f.svc.Evidence(f.ctx, FamilyHygiene, ClassIndexDrop)
	if err != nil || ev.Record == nil || ev.Record.Regressed != 1 ||
		ev.Record.LastDemeritAt == nil || ev.Record.LastDemerit != CauseRegressed {
		t.Fatalf("evidence = %+v (%v)", ev.Record, err)
	}
}

// Incident-family actions keep their own path; the self pass skips them.
func TestSelfReconcileSkipsIncidentFamilyActions(t *testing.T) {
	f := newSelfFixture(t)
	id := f.l3Decision("wraparound_runway", "freeze", "success")
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO sage.action_outcome
		(action_log_id, action_class, predicted, prediction_method, verdict, tolerance,
		 decided_at) VALUES ($1, 'vacuum', '{}', 'rule', 'improved', 'met', now())`,
		id); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.selfOutcomes()[id] {
		if r.family == string(FamilyHygiene) {
			t.Fatalf("the incident action was recorded as hygiene: %+v", r)
		}
	}
}

// A failed notification never undoes the demotion; the failure is
// reported.
func TestSelfReconcileNotifierFailureKeepsTheDemotion(t *testing.T) {
	f := newSelfFixture(t)
	f.grandfatherAt(L3)
	f.selfAction("analyze_table", "ANALYZE public.orders", "analyze", "success",
		"regressed", "autonomy_l3", 0)
	n := &demotionNotifier{fail: true}
	_, err := NewReconciler(f.svc, f.pool, f.db, n).RunOnce(f.ctx)
	if err == nil || !strings.Contains(err.Error(), "chat webhook down") {
		t.Fatalf("notification failure not reported: %v", err)
	}
	if got := f.state(FamilyHygiene, ClassAnalyze).Level; got != L2 {
		t.Fatalf("analyze = %v after a regression, want L2", got)
	}
}

// Promotion: the floor (the old ramp) plus verified successes since the
// last demerit lets pg_sage propose; an admin approves.
func TestSelfEvaluateProposesOnEvidenceAndFloor(t *testing.T) {
	f := newSelfFixture(t)
	now := f.clock.Now()
	f.svc.WithRamp(func() RampFloor {
		return RampFloor{Start: now.Add(-10 * 24 * time.Hour)}
	})
	for i := 0; i < 3; i++ {
		f.selfAction("analyze_table", "ANALYZE public.orders", "analyze", "success",
			"improved", "operator_approved", time.Duration(i)*time.Minute)
	}
	if _, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	e, err := f.svc.Evaluate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var proposal *Proposal
	for i := range e.Created {
		if e.Created[i].Family == FamilyHygiene && e.Created[i].Class == ClassAnalyze {
			proposal = &e.Created[i]
		}
	}
	if proposal == nil || proposal.From != L1 || proposal.To != L2 {
		t.Fatalf("created = %+v", e.Created)
	}
	for _, np := range e.NotProposed {
		if np.Family == FamilyHygiene && np.Class == ClassVacuum {
			if np.Reason != NotProposedEvidence || !hasUnmet(np.Unmet, "class_successes") {
				t.Fatalf("vacuum not proposed = %+v", np)
			}
		}
	}
	st, err := f.svc.Approve(f.ctx, proposal.ID, "user:1:admin@example.com", "earned")
	if err != nil || st.Level != L2 {
		t.Fatalf("approve = %+v (%v)", st, err)
	}
}

func TestSelfEvaluateWaitsForTheFloor(t *testing.T) {
	f := newSelfFixture(t)
	now := f.clock.Now()
	f.svc.WithRamp(func() RampFloor {
		return RampFloor{Start: now.Add(-time.Hour), Safe: 4 * time.Hour}
	})
	for i := 0; i < 5; i++ {
		f.selfAction("analyze_table", "ANALYZE public.orders", "analyze", "success",
			"improved", "operator_approved", time.Minute)
	}
	if _, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	e, err := f.svc.Evaluate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range e.Created {
		if IsSelfInitiated(p.Family) {
			t.Fatalf("proposed before the floor: %+v", p)
		}
	}
	for _, np := range e.NotProposed {
		if np.Family == FamilyHygiene && np.Class == ClassAnalyze {
			c, ok := unmetCheck(np.Unmet, "observation_floor")
			want := now.Add(3 * time.Hour)
			if !ok || c.ETA == nil || !c.ETA.Equal(want) {
				t.Fatalf("floor check = %+v, want ETA %v", c, want)
			}
		}
	}
}

// Without a ramp source the floor is unknown and nothing is proposed
// (fail closed).
func TestSelfEvaluateWithoutRampProposesNothing(t *testing.T) {
	f := newSelfFixture(t)
	for i := 0; i < 12; i++ {
		f.selfAction("vacuum_table", "VACUUM public.orders", "vacuum", "success",
			"improved", "operator_approved", time.Minute)
	}
	if _, err := NewReconciler(f.svc, f.pool, f.db, nil).RunOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	e, err := f.svc.Evaluate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range e.Created {
		if IsSelfInitiated(p.Family) {
			t.Fatalf("proposed without a known floor: %+v", p)
		}
	}
}

func hasUnmet(checks []Check, name string) bool {
	_, ok := unmetCheck(checks, name)
	return ok
}

func unmetCheck(checks []Check, name string) (Check, bool) {
	for _, c := range checks {
		if c.Name == name && !c.Met {
			return c, true
		}
	}
	return Check{}, false
}
