package earned

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Dogfood round 2 (item 3): sage.trust_ledger_state had a 75% dead-tuple
// ratio on lifeos (1 live row, 3 dead) because every reconcile pass
// rewrote the database's cursor row even when no cursor moved. A pass that
// reads nothing new must not write a new row version.

// ledgerStateRow is the row's physical identity: a rewrite (update) gives
// it a new ctid or xmin, so an unchanged pair proves no dead tuple was made.
func ledgerStateRow(t *testing.T, f *fixture) (string, string) {
	t.Helper()
	var ctid, xmin string
	err := f.pool.QueryRow(f.ctx, `SELECT ctid::text, xmin::text
		FROM sage.trust_ledger_state WHERE deployment_id = $1 AND database_name = $2`,
		f.store.DeploymentID(), f.db).Scan(&ctid, &xmin)
	if err != nil {
		t.Fatalf("read trust_ledger_state row: %v", err)
	}
	return ctid, xmin
}

// tableUpdates is n_tup_upd of a sage table once the statistics are
// flushed (pg_stat_force_next_flush on PG15+; PG14 reports through the
// collector, so it is polled until two reads agree).
func tableUpdates(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) int64 {
	t.Helper()
	_, _ = pool.Exec(ctx, `DO $$ BEGIN
		IF current_setting('server_version_num')::int >= 150000 THEN
			PERFORM pg_stat_force_next_flush();
		END IF; END $$`)
	var prev int64 = -1
	for i := 0; i < 20; i++ {
		var n int64
		if err := pool.QueryRow(ctx, `SELECT n_tup_upd FROM pg_stat_user_tables
			WHERE relid = to_regclass($1)`, "sage."+table).Scan(&n); err != nil {
			t.Fatalf("read n_tup_upd of %s: %v", table, err)
		}
		if n == prev {
			return n
		}
		prev = n
		time.Sleep(250 * time.Millisecond)
	}
	return prev
}

func TestReconcileWithoutNewEvidenceDoesNotRewriteLedgerState(t *testing.T) {
	f := newSelfFixture(t)
	f.selfAction("vacuum_table", "VACUUM public.orders", "vacuum", "success", "neutral",
		"operator_approved", time.Minute)
	rec := NewReconciler(f.svc, f.pool, f.db, nil)
	if _, err := rec.RunOnce(f.ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	ctid, xmin := ledgerStateRow(t, f)
	updatesBefore := tableUpdates(t, f.ctx, f.pool, "trust_ledger_state")

	const cycles = 25
	for i := 0; i < cycles; i++ {
		if _, err := rec.RunOnce(f.ctx); err != nil {
			t.Fatalf("pass %d: %v", i+2, err)
		}
	}
	gotCtid, gotXmin := ledgerStateRow(t, f)
	if gotCtid != ctid || gotXmin != xmin {
		t.Fatalf("%d passes without new evidence rewrote the cursor row "+
			"(ctid %s -> %s, xmin %s -> %s): every rewrite is a dead tuple",
			cycles, ctid, gotCtid, xmin, gotXmin)
	}
	if delta := tableUpdates(t, f.ctx, f.pool, "trust_ledger_state") - updatesBefore; delta > 0 {
		// The package has its own fixture database and its tests run one at
		// a time, so every update of the table is this test's.
		t.Fatalf("%d passes without new evidence made %d updates", cycles, delta)
	}
}

// The guard must not freeze the cursors: new evidence still moves them.
func TestReconcileWithNewEvidenceStillAdvancesTheCursor(t *testing.T) {
	f := newSelfFixture(t)
	f.selfAction("vacuum_table", "VACUUM public.orders", "vacuum", "success", "neutral",
		"operator_approved", 10*time.Minute)
	rec := NewReconciler(f.svc, f.pool, f.db, nil)
	if _, err := rec.RunOnce(f.ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	before, err := f.store.cursors(f.ctx)
	if err != nil || before.verdict == nil {
		t.Fatalf("cursor after first pass = %+v (%v)", before, err)
	}
	f.selfAction("analyze_table", "ANALYZE public.orders", "analyze", "success",
		"improved", "operator_approved", time.Minute)
	res, err := rec.RunOnce(f.ctx)
	if err != nil || res.SelfRecorded != 1 {
		t.Fatalf("second pass = %+v (%v), want the new verdict recorded", res, err)
	}
	after, err := f.store.cursors(f.ctx)
	if err != nil || after.verdict == nil || !after.verdict.After(*before.verdict) {
		t.Fatalf("verdict cursor %v -> %v: new evidence must advance it (%v)",
			before.verdict, after.verdict, err)
	}
}

// The cursors never move back, guard or not.
func TestSaveCursorsNeverMovesBack(t *testing.T) {
	f := newSelfFixture(t)
	late := time.Now().UTC().Truncate(time.Microsecond)
	early := late.Add(-time.Hour)
	if err := f.store.saveCursors(f.ctx, selfCursors{verdict: &late}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := f.store.saveCursors(f.ctx, selfCursors{verdict: &early,
		rollback: &early}); err != nil {
		t.Fatalf("save older: %v", err)
	}
	got, err := f.store.cursors(f.ctx)
	if err != nil || got.verdict == nil || !got.verdict.Equal(late) || got.rollback == nil ||
		!got.rollback.Equal(early) {
		t.Fatalf("cursors = %+v (%v), want verdict kept at %v and rollback set to %v",
			got, err, late, early)
	}
}
