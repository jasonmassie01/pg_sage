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

// tableUpdates is n_tup_upd of a sage table once every backend's counts
// have reached the statistics view. PG15+ flushes on request
// (pg_stat_force_next_flush). PG14 reports through the collector, and a
// backend that reported less than PGSTAT_STAT_INTERVAL (500 ms) ago keeps
// its counts pending until its next transaction, so a pooled connection
// may report the first pass's update in the middle of the later passes
// (CI integration-matrix 14). There the pool is reset (an exiting backend
// reports what it holds) and the value must hold across reads spanning
// at least statsSettle. The row's ctid/xmin stays the primary proof.
func tableUpdates(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string) int64 {
	t.Helper()
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatalf("read server version: %v", err)
	}
	if version >= 150000 {
		if _, err := pool.Exec(ctx, "SELECT pg_stat_force_next_flush()"); err != nil {
			t.Fatalf("flush statistics: %v", err)
		}
		return readTableUpdates(t, ctx, pool, table)
	}
	pool.Reset()
	return settledTableUpdates(t, ctx, pool, table)
}

// statsSettle is how long a PG14 reading must stay unchanged: three
// collector reporting intervals.
const statsSettle = 1500 * time.Millisecond

func settledTableUpdates(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	table string) int64 {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	value, since := readTableUpdates(t, ctx, pool, table), time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		n := readTableUpdates(t, ctx, pool, table)
		if n != value {
			value, since = n, time.Now()
			continue
		}
		if time.Since(since) >= statsSettle {
			return value
		}
	}
	t.Fatalf("n_tup_upd of sage.%s never settled for %s", table, statsSettle)
	return 0
}

func readTableUpdates(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	table string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, `SELECT n_tup_upd FROM pg_stat_user_tables
		WHERE relid = to_regclass($1)`, "sage."+table).Scan(&n); err != nil {
		t.Fatalf("read n_tup_upd of %s: %v", table, err)
	}
	return n
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
