package outcomes

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// ux-02a on real pg_sage tables: one row per executed change, joined with
// its verdict record, decision, approval card and plan step.

var testNow = time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)

func outcomesDB(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

func id(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func action(t *testing.T, pool *pgxpool.Pool, at time.Time, actionType, outcome,
	rollback string) int64 {
	t.Helper()
	var rb any
	if rollback != "" {
		rb = rollback
	}
	return id(t, pool, `INSERT INTO sage.action_log (executed_at, action_type, sql_executed,
		rollback_sql, outcome) VALUES ($1, $2, 'CREATE INDEX CONCURRENTLY i ON t (a)', $3, $4)
		RETURNING id`, at, actionType, rb, outcome)
}

func verdict(t *testing.T, pool *pgxpool.Pool, actionID int64, v, observed, reason string) {
	t.Helper()
	exec(t, pool, `INSERT INTO sage.action_outcome (action_log_id, action_class, predicted,
		prediction_method, verdict, observed, reason) VALUES ($1, 'index_create',
		'{"class":"index_create","method":"hypopg","metric":"blocks_per_call",
		  "expected_change_pct":-40}', 'hypopg', $2, $3::jsonb, $4)
		ON CONFLICT (action_log_id) DO UPDATE SET verdict = EXCLUDED.verdict,
		observed = EXCLUDED.observed, reason = EXCLUDED.reason`, actionID, v, observed, reason)
}

func read(t *testing.T, pool *pgxpool.Pool, q Query) Result {
	t.Helper()
	if q.Now.IsZero() {
		q.Now = testNow
	}
	if q.Since == 0 {
		q.Since = DefaultSince
	}
	if q.Limit == 0 {
		q.Limit = DefaultLimit
	}
	res, err := Read(context.Background(), pool, "orders", q)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return res
}

const measured42 = `{"metric":"blocks_per_call","before":100,"after":58,"change_pct":-42}`

func TestOutcomeLedgerDB_OneRowWithEveryField(t *testing.T) {
	pool := outcomesDB(t, "outcomes_happy")
	finding := id(t, pool, `INSERT INTO sage.findings (category, severity, title, detail,
		status, object_identifier) VALUES ('missing_index', 'warning', 'Seq scans on orders',
		'{}', 'open', 'public.orders') RETURNING id`)
	act := action(t, pool, testNow.Add(-2*time.Hour), "create_index", "success",
		"DROP INDEX CONCURRENTLY i")
	exec(t, pool, `UPDATE sage.action_log SET finding_id = $1, approved_by = 7,
		toil_minutes_saved = 45 WHERE id = $2`, finding, act)
	exec(t, pool, `INSERT INTO sage.action_queue (finding_id, proposed_sql, action_risk,
		status, action_type, reason, action_log_id) VALUES ($1, 'CREATE INDEX', 'moderate',
		'executed', 'create_index', 'orders lookups read 1.2 GB per call', $2)`, finding, act)
	exec(t, pool, `INSERT INTO sage.decision (feature, intent, verdict, risk_tier, reason,
		evidence_id, action_log_id) VALUES ('index', 'create_index', 'queue_approval',
		'moderate', 'policy', 'ev-orders-1', $1)`, act)
	plan := id(t, pool, `INSERT INTO sage.plan (goal, origin, status, plan_hash, created_by,
		pack_hash) VALUES ('speed up orders', 'person', 'running', repeat('a', 64), 'alice',
		repeat('b', 64)) RETURNING id`)
	exec(t, pool, `INSERT INTO sage.plan_step (plan_id, ordinal, kind, metric,
		expected_direction, expected_source, deadline_minutes, on_improved, on_no_change,
		on_regressed, on_inconclusive, on_expired, action_log_id, expected_pct) VALUES
		($1, 1, 'action', 'blocks_per_call', 'down', 'hypopg', 60, 'keep', 'keep',
		'revert_step', 'halt_ask', 'halt_ask', $2, 30)`, plan, act)
	verdict(t, pool, act, "improved", measured42, "")

	res := read(t, pool, Query{})
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %+v, want one", res.Rows)
	}
	r := res.Rows[0]
	if r.ActionID != act || r.Database != "orders" || r.What != "Add an index" ||
		r.ActionType != "create_index" || r.Why != "orders lookups read 1.2 GB per call" ||
		r.Origin != OriginPlan || r.Verdict != VerdictImproved ||
		r.RollbackState != RollbackAvailable || r.EvidenceID != "ev-orders-1" ||
		r.PackHash != strings.Repeat("b", 64) || r.PlanID == nil || *r.PlanID != plan ||
		!r.ExecutedAt.Equal(testNow.Add(-2*time.Hour)) || r.Tier != "" {
		t.Fatalf("row = %+v", r)
	}
	if r.Expected == nil || r.Expected.ChangePct != -40 || r.Expected.Source != "hypopg" {
		t.Fatalf("expected = %+v, want the stored prediction (-40%%)", r.Expected)
	}
	if r.Measured == nil || r.Measured.ChangePct != -42 ||
		r.Measured.Metric != "blocks_per_call" || r.MeasuredReason != "" {
		t.Fatalf("measured = %+v %q", r.Measured, r.MeasuredReason)
	}
	if r.SavedHours == nil || *r.SavedHours != 0.75 {
		t.Fatalf("saved hours = %v, want 0.75", r.SavedHours)
	}
	if res.Summary.Changes != 1 || res.Summary.Verified != 1 ||
		res.Summary.MeasuredShare == nil || *res.Summary.MeasuredShare != 1 {
		t.Fatalf("summary = %+v", res.Summary)
	}
}

func TestOutcomeLedgerDB_EmptyPendingAndOrigins(t *testing.T) {
	pool := outcomesDB(t, "outcomes_empty")
	res := read(t, pool, Query{})
	if res.Rows == nil || len(res.Rows) != 0 || res.Summary.MeasuredShare != nil ||
		res.Errors == nil {
		t.Fatalf("no actions: %+v, want rows [] and a null share", res)
	}
	auto := action(t, pool, testNow.Add(-time.Hour), "analyze_table", "success", "")
	agent := action(t, pool, testNow.Add(-2*time.Hour), "vacuum", "success", "")
	exec(t, pool, `UPDATE sage.action_log SET principal_id = 'agp_x' WHERE id = $1`, agent)
	action(t, pool, testNow.Add(-3*time.Hour), "create_index", "failed", "DROP INDEX i")
	res = read(t, pool, Query{})
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %+v, want the two changes (a failed one changed nothing)", res.Rows)
	}
	a, g := res.Rows[0], res.Rows[1]
	if a.ActionID != auto || a.Verdict != VerdictPending || a.Measured != nil ||
		a.MeasuredReason == "" || a.RollbackState != RollbackNotReversible ||
		a.Origin != OriginAutonomous || a.Expected != nil || a.Why != "" {
		t.Fatalf("unverified row = %+v", a)
	}
	if g.ActionID != agent || g.Origin != OriginAgent || g.What != "Clean up expired table rows" {
		t.Fatalf("agent row = %+v", g)
	}
	if res.Summary.Pending != 2 || res.Summary.Measured != 0 ||
		res.Summary.MeasuredShare == nil || *res.Summary.MeasuredShare != 0 {
		t.Fatalf("summary = %+v, want 0 of 2 measured", res.Summary)
	}
}

func TestOutcomeLedgerDB_ClosedPoolNamesTheDatabase(t *testing.T) {
	pool := outcomesDB(t, "outcomes_closed")
	pool.Close()
	_, err := Read(context.Background(), pool, "orders", Query{Since: DefaultSince,
		Now: testNow, Limit: 10})
	if err == nil || !strings.HasPrefix(err.Error(), "read outcomes: orders: ") {
		t.Fatalf("err = %v, want read outcomes: orders: ...", err)
	}
}

func TestOutcomeLedgerDB_WindowBoundaryAndOneThird(t *testing.T) {
	pool := outcomesDB(t, "outcomes_window")
	edge := action(t, pool, testNow.Add(-24*time.Hour), "create_index", "success", "x")
	action(t, pool, testNow.Add(-24*time.Hour-time.Second), "create_index", "success", "x")
	mid := action(t, pool, testNow.Add(-time.Hour), "reindex", "success", "")
	action(t, pool, testNow.Add(-2*time.Hour), "vacuum", "success", "")
	verdict(t, pool, edge, "improved", measured42, "")
	verdict(t, pool, mid, "unverifiable", `{}`, "suspend epoch break")
	res := read(t, pool, Query{Since: 24 * time.Hour})
	if len(res.Rows) != 3 || res.Rows[2].ActionID != edge {
		t.Fatalf("rows = %+v, want three with the 24 h edge last", res.Rows)
	}
	if *res.Summary.MeasuredShare != 0.333 || res.Summary.Inconclusive != 1 {
		t.Fatalf("summary = %+v, want 0.333", res.Summary)
	}
	if r := res.Rows[0]; r.Verdict != VerdictUnverifiable || r.Measured != nil ||
		r.MeasuredReason != "suspend epoch break" {
		t.Fatalf("unverifiable row = %+v, want no measurement and lab-01's reason", r)
	}
	limited := read(t, pool, Query{Limit: 1})
	if len(limited.Rows) != 1 || limited.Summary.Changes != 3 || limited.Truncated {
		t.Fatalf("limit 1: %+v, want one row and a summary of all three", limited)
	}
	only := read(t, pool, Query{Verdict: VerdictImproved})
	if len(only.Rows) != 1 || only.Rows[0].ActionID != edge || only.Summary.Changes != 1 {
		t.Fatalf("verdict filter: %+v", only)
	}
}

func TestOutcomeLedgerDB_StateTransitions(t *testing.T) {
	pool := outcomesDB(t, "outcomes_states")
	act := action(t, pool, testNow.Add(-time.Hour), "create_index", "success", "DROP INDEX i")
	step := func(want, rollback string) {
		t.Helper()
		r := read(t, pool, Query{}).Rows[0]
		if r.Verdict != want || r.RollbackState != rollback {
			t.Fatalf("row = %s/%s, want %s/%s", r.Verdict, r.RollbackState, want, rollback)
		}
	}
	verdict(t, pool, act, "pending", `{}`, "")
	step(VerdictPending, RollbackAvailable)
	verdict(t, pool, act, "improved", measured42, "")
	step(VerdictImproved, RollbackAvailable)
	exec(t, pool, `UPDATE sage.action_log SET outcome = 'rolled_back' WHERE id = $1`, act)
	step(VerdictImproved, RollbackRolledBack)

	reg := action(t, pool, testNow.Add(-30*time.Minute), "create_index", "success", "DROP")
	verdict(t, pool, reg, "regressed", `{"metric":"time_per_call","before":2,"after":3}`, "")
	exec(t, pool, `UPDATE sage.action_log SET outcome = 'rolled_back',
		rollback_reason = 'regressed' WHERE id = $1`, reg)
	r := read(t, pool, Query{}).Rows[0]
	if r.ActionID != reg || r.Verdict != VerdictRegressed || r.RollbackState != RollbackRolledBack ||
		r.Measured == nil || r.Measured.ChangePct != 50 {
		t.Fatalf("auto-reverted row = %+v", r)
	}
	if s := read(t, pool, Query{}).Summary; s.Reverted != 2 || s.Verified != 2 {
		t.Fatalf("summary = %+v, want two reverted, two verified", s)
	}
}

func TestOutcomeLedgerDB_EvidenceFilterFindsDecisionsAndIncidents(t *testing.T) {
	pool := outcomesDB(t, "outcomes_evidence")
	a := action(t, pool, testNow.Add(-time.Hour), "create_index", "success", "x")
	b := action(t, pool, testNow.Add(-2*time.Hour), "vacuum", "success", "")
	dec := id(t, pool, `INSERT INTO sage.decision (feature, intent, verdict, risk_tier,
		reason, evidence_id) VALUES ('index', 'create_index', 'execute', 'safe', 'ok',
		'ev-dec') RETURNING id`)
	exec(t, pool, `UPDATE sage.action_log SET decision_id = $1 WHERE id = $2`, dec, a)
	ver := id(t, pool, `INSERT INTO sage.verification (decision_id, action_log_id, criterion,
		baseline, minimum_samples, next_evaluation_at, hard_deadline_at, verdict)
		VALUES ($1, $2, '{}', '{}', 1, now(), now(), 'success') RETURNING id`, dec, b)
	exec(t, pool, `INSERT INTO sage.incident_avoided (kind, severity, credited_minutes,
		evidence_id, model_version, decision_id, action_log_id, verification_id,
		occurred_at) VALUES ('xid_wraparound', 'near_miss', 30, 'ev-incident', 1, $1, $2, $3, now())`,
		dec, b, ver)
	if r := read(t, pool, Query{Evidence: "ev-dec"}); len(r.Rows) != 1 ||
		r.Rows[0].ActionID != a || r.Rows[0].EvidenceID != "ev-dec" {
		t.Fatalf("decision evidence: %+v", r.Rows)
	}
	if r := read(t, pool, Query{Evidence: "ev-incident"}); len(r.Rows) != 1 ||
		r.Rows[0].ActionID != b {
		t.Fatalf("incident evidence: %+v", r.Rows)
	}
	if r := read(t, pool, Query{Evidence: "nope"}); len(r.Rows) != 0 {
		t.Fatalf("unknown evidence: %+v", r.Rows)
	}
}

// A verification completing while the ledger is read gives the old or the
// new verdict, never a decided verdict without its measurement: the row is
// one statement's snapshot.
func TestOutcomeLedgerDB_ConcurrentVerdictIsOneSnapshot(t *testing.T) {
	pool := outcomesDB(t, "outcomes_race")
	act := action(t, pool, testNow.Add(-time.Hour), "create_index", "success", "x")
	verdict(t, pool, act, "pending", `{}`, "")
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			v, obs := "pending", `{}`
			if i%2 == 0 {
				v, obs = "improved", measured42
			}
			if _, err := pool.Exec(context.Background(), `UPDATE sage.action_outcome
				SET verdict = $2, observed = $3::jsonb WHERE action_log_id = $1`,
				act, v, obs); err != nil {
				t.Errorf("flip verdict: %v", err)
				return
			}
		}
	}()
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		r := read(t, pool, Query{}).Rows[0]
		seen[r.Verdict]++
		if r.Verdict == VerdictImproved && r.Measured == nil {
			t.Fatalf("read %d: improved with no measurement", i)
		}
		if r.Verdict == VerdictPending && r.Measured != nil {
			t.Fatalf("read %d: pending with a measurement", i)
		}
	}
	stop.Store(true)
	wg.Wait()
	if seen[VerdictImproved]+seen[VerdictPending] != 200 {
		t.Fatalf("verdicts seen = %v", seen)
	}
}
