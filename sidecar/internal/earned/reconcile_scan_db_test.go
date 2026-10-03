package earned

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// Performance gate offender 3: every reconcile pass hash-joined every
// executed decision of the ledger with action_log, reading all of
// sage.decision (and of sage.action_queue for handoffs) each cycle. The
// reconciler's reads must be bounded by the autonomous actions there
// are, not by the ledger's history: the history below is the gate's
// (20,000 decisions over 60 days, half of the actions linked to one).

func seedLedgerHistory(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(ctx, `INSERT INTO sage.decision (feature, intent,
		target_objects, verdict, risk_tier, reason, evidence, evidence_id, created_at)
		SELECT 'executor', 'retention_cleanup', '[]',
		       (ARRAY['execute','queue_approval','parked','blocked','observe_only'])
		       [1 + g % 5], 'safe', 'perf history',
		       jsonb_build_object('disposition', 'apply'), 'perf-hist-' || g,
		       now() - (g::float8 / 20000) * interval '60 days'
		FROM generate_series(1, 20000) g;
		INSERT INTO sage.action_log (executed_at, action_type, sql_executed, outcome,
		  decision_id)
		SELECT now() - (g::float8 / 20000) * interval '60 days', 'vacuum', 'SELECT 1',
		       (ARRAY['success','failed','rolled_back'])[1 + g % 3],
		       CASE WHEN g % 2 = 0 THEN d.first + g - 1 END
		FROM generate_series(1, 20000) g,
		     (SELECT min(id) AS first FROM sage.decision
		       WHERE evidence_id LIKE 'perf-hist-%') d;
		INSERT INTO sage.action_queue (proposed_sql, action_risk, status, identity_key,
		  action_log_id)
		SELECT 'VACUUM t', 'safe', 'approved', 'finding:perf:' || l.id, l.id
		FROM sage.action_log l WHERE l.sql_executed = 'SELECT 1'
		  AND l.action_type = 'vacuum' LIMIT 6000;
		ANALYZE sage.decision, sage.action_log, sage.action_queue`); err != nil {
		t.Fatalf("seed ledger history: %v", err)
	}
}

func TestReconcile_ReadsBoundedByAutonomousActions(t *testing.T) {
	f := newFixture(t)
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	seedLedgerHistory(t, f.ctx, tx)
	var l3 int64
	if err := tx.QueryRow(f.ctx, `WITH d AS (
		INSERT INTO sage.decision (feature, intent, target_objects, verdict, risk_tier,
		  reason, evidence, evidence_id)
		VALUES ('freeze', 'freeze', '["public.orders"]', 'execute', 'safe', 'autonomy_l3',
		  '{"incident_family": "wraparound_runway", "autonomy_class": "vacuum_freeze"}',
		  md5(random()::text)) RETURNING id)
		INSERT INTO sage.action_log (action_type, sql_executed, outcome, decision_id)
		SELECT 'vacuum_table', 'VACUUM (FREEZE) public.orders', 'success', id FROM d
		RETURNING id`).Scan(&l3); err != nil {
		t.Fatalf("seed L3 execution: %v", err)
	}
	before, err := testdb.XactScansOf(f.ctx, tx, "sage.decision")
	if err != nil {
		t.Fatal(err)
	}
	ids := readActionIDs(t, f.ctx, tx, autoExecutionSQL)
	after, err := testdb.XactScansOf(f.ctx, tx, "sage.decision")
	if err != nil {
		t.Fatal(err)
	}
	if !ids[l3] {
		t.Fatalf("autonomous executions = %v, want action %d among them", ids, l3)
	}
	if d := after.Minus(before); d.Seq != 0 || d.IndexFetch > int64(2*len(ids)+5) {
		t.Fatalf("reading 1 autonomous execution scanned sage.decision: %+v", d)
	}
}

func TestReconcile_HandoffReadsBoundedByHandoffs(t *testing.T) {
	f := newFixture(t)
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	seedLedgerHistory(t, f.ctx, tx)
	var handoff int64
	if err := tx.QueryRow(f.ctx, `WITH l AS (
		INSERT INTO sage.action_log (action_type, sql_executed, outcome)
		VALUES ('vacuum_table', 'VACUUM (FREEZE) public.orders', 'success') RETURNING id)
		INSERT INTO sage.action_queue (proposed_sql, action_risk, status, identity_key,
		  action_log_id)
		SELECT 'VACUUM (FREEZE) public.orders', 'safe', 'approved',
		       'autonomy:wraparound_runway:vacuum_freeze:public.orders', id FROM l
		RETURNING action_log_id`).Scan(&handoff); err != nil {
		t.Fatalf("seed handoff: %v", err)
	}
	before, err := testdb.XactScansOf(f.ctx, tx, "sage.action_queue")
	if err != nil {
		t.Fatal(err)
	}
	ids := readActionIDs(t, f.ctx, tx, handoffSQL)
	after, err := testdb.XactScansOf(f.ctx, tx, "sage.action_queue")
	if err != nil {
		t.Fatal(err)
	}
	if !ids[handoff] {
		t.Fatalf("handoffs = %v, want action %d among them", ids, handoff)
	}
	if d := after.Minus(before); d.Seq != 0 || d.IndexFetch > int64(2*len(ids)+5) {
		t.Fatalf("reading 1 handoff scanned sage.action_queue: %+v", d)
	}
}

// readActionIDs runs a reconcile read (lookback 30 days, settled after
// UnverifiedAfter) and returns the action_log ids it found (the column
// named id). Other tests' committed autonomous actions may be among them.
func readActionIDs(t *testing.T, ctx context.Context, tx pgx.Tx, sql string) map[int64]bool {
	t.Helper()
	rows, err := tx.Query(ctx, sql, (30 * 24 * 3600.0), UnverifiedAfter.Seconds())
	if err != nil {
		t.Fatalf("reconcile read: %v", err)
	}
	defer rows.Close()
	col := -1
	for i, fd := range rows.FieldDescriptions() {
		if fd.Name == "id" {
			col = i
		}
	}
	if col < 0 {
		t.Fatal("reconcile read has no id column")
	}
	out := map[int64]bool{}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		id, ok := vals[col].(int64)
		if !ok {
			t.Fatalf("id column is %T", vals[col])
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
