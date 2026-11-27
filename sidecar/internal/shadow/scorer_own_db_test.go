package shadow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration (real Postgres), earned-03: a change pg_sage made itself is
// never evidence "applied outside pg_sage", even when the pass's fact
// window (factBatch actions from the oldest pending decision) ends before
// pg_sage's own action. Such a decision stays unscored at the horizon,
// and only a change no pg_sage action explains is counted as external.

// shadowTable creates public.name (a int, b int), dropped after the test.
func shadowTable(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	mustExec(t, pool, `DROP TABLE IF EXISTS public.`+name)
	mustExec(t, pool, `CREATE TABLE public.`+name+` (a int, b int)`)
	t.Cleanup(func() { mustExec(t, pool, `DROP TABLE IF EXISTS public.`+name) })
}

// busyActions records n successful vacuums run about ago (a busy
// database), enough to push later actions out of one pass's window.
func busyActions(t *testing.T, pool *pgxpool.Pool, n int, ago time.Duration) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, executed_at)
		SELECT 'vacuum', 'VACUUM public.o', 'success',
		       now() - make_interval(secs => $2) + g * interval '1 millisecond'
		FROM generate_series(1, $1) g`, n, ago.Seconds())
}

// ownActions records n of pg_sage's successful action_type actions on
// public.table, each of another shape than (a), run about ago.
func ownActions(t *testing.T, pool *pgxpool.Pool, actionType, table string, n int,
	ago time.Duration) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, executed_at)
		SELECT $1, 'CREATE INDEX CONCURRENTLY other_' || g || ' ON public.' || $2 ||
		       ' (b) WHERE a > ' || g, 'success',
		       now() - make_interval(secs => $4) + g * interval '1 millisecond'
		FROM generate_series(1, $3) g`, actionType, table, n, ago.Seconds())
}

// assertNotApplied: the scorer did not take d's change for one made
// outside pg_sage, and d is still pending.
func assertNotApplied(t *testing.T, s *Store, id int64) {
	t.Helper()
	d := get(t, s, id)
	if d.AppliedDetectedAt != nil || d.AppliedAfter != nil || d.Status != StatusPending {
		t.Fatalf("decision %d: status=%s applied_after=%v applied_detected_at=%v, "+
			"want pending and not applied outside pg_sage", id, d.Status, d.AppliedAfter,
			d.AppliedDetectedAt)
	}
}

func assertApplied(t *testing.T, s *Store, id int64) {
	t.Helper()
	d := get(t, s, id)
	if d.AppliedDetectedAt == nil || d.AppliedAfter == nil || d.Status != StatusPending {
		t.Fatalf("decision %d: status=%s applied_detected_at=%v, want detected as applied "+
			"outside pg_sage", id, d.Status, d.AppliedDetectedAt)
	}
}

// unscoredAtHorizon ages d past the horizon and scores it: no evidence.
func unscoredAtHorizon(t *testing.T, pool *pgxpool.Pool, s *Store, id int64) {
	t.Helper()
	o := fastOptions()
	age(t, pool, id, o.Horizon+time.Minute)
	runOnce(t, newScorer(pool, o))
	assertScore(t, s, id, ScoreUnscored, SourceNone, false)
}

// Failed before earned-03: 2,100 vacuums filled the pass's action window,
// so pg_sage's own CREATE INDEX (after an approval) was not matched, the
// catalog showed the index, and it was marked applied outside pg_sage,
// then counted as external evidence.
func TestScorerOwnIndexBeyondTheFactWindowIsNotExternal(t *testing.T) {
	pool, _ := testPool(t)
	shadowTable(t, pool, "shadow_own")
	s := NewStore(pool)
	d := record(t, s, sample("index_create",
		`CREATE INDEX CONCURRENTLY i_own ON public.shadow_own (a)`))
	age(t, pool, d.ID, 3*time.Hour)
	busyActions(t, pool, factBatch+100, 2*time.Hour)
	mustExec(t, pool, `CREATE INDEX shadow_own_sage ON public.shadow_own (a)`)
	executedAction(t, pool, "create_index",
		`CREATE INDEX CONCURRENTLY shadow_own_sage ON public.shadow_own (a)`,
		"success", "improved", time.Hour)
	runOnce(t, newScorer(pool, fastOptions()))
	assertNotApplied(t, s, d.ID)
	unscoredAtHorizon(t, pool, s, d.ID)
	// Acceptance CHECK-03: no counted external score has a pg_sage index
	// create after its decision.
	var external int
	if err := pool.QueryRow(context.Background(), `SELECT count(*)
		FROM sage.shadow_decision d JOIN sage.action_log l
		  ON l.executed_at >= d.recorded_at AND l.action_type = 'create_index'
		WHERE d.score_source = 'external' AND d.counted`).
		Scan(&external); err != nil || external != 0 {
		t.Fatalf("counted external scores = %d (%v), want 0", external, err)
	}
}

// Failed before: pg_sage's own drop, past the window, made the index look
// dropped by someone else.
func TestScorerOwnDropBeyondTheFactWindowIsNotExternal(t *testing.T) {
	pool, _ := testPool(t)
	shadowTable(t, pool, "shadow_owndrop")
	mustExec(t, pool, `CREATE INDEX shadow_owndrop_a ON public.shadow_owndrop (a)`)
	s := NewStore(pool)
	drop := sample("index_drop", `DROP INDEX CONCURRENTLY public.shadow_owndrop_a`)
	drop.RollbackSQL = `CREATE INDEX shadow_owndrop_a ON public.shadow_owndrop USING btree (a);`
	d := record(t, s, drop)
	age(t, pool, d.ID, 3*time.Hour)
	busyActions(t, pool, factBatch+100, 2*time.Hour)
	mustExec(t, pool, `DROP INDEX public.shadow_owndrop_a`)
	executedAction(t, pool, "drop_index",
		`DROP INDEX CONCURRENTLY IF EXISTS public."shadow_owndrop_a";`, "success", "", time.Hour)
	runOnce(t, newScorer(pool, fastOptions()))
	assertNotApplied(t, s, d.ID)
	unscoredAtHorizon(t, pool, s, d.ID)
}

// Failed before: the new index of pg_sage's own replacement (one action,
// two statements) has the decision's shape.
func TestScorerOwnReplaceBeyondTheFactWindowIsNotExternal(t *testing.T) {
	pool, _ := testPool(t)
	shadowTable(t, pool, "shadow_repl")
	s := NewStore(pool)
	d := record(t, s, sample("index_create",
		`CREATE INDEX CONCURRENTLY i_repl ON public.shadow_repl (a, b)`))
	age(t, pool, d.ID, 3*time.Hour)
	busyActions(t, pool, factBatch+100, 2*time.Hour)
	mustExec(t, pool, `CREATE INDEX shadow_repl_ab ON public.shadow_repl (a, b)`)
	executedAction(t, pool, "replace_index",
		`CREATE INDEX CONCURRENTLY shadow_repl_ab ON public.shadow_repl (a, b); `+
			`DROP INDEX CONCURRENTLY public.shadow_repl_a;`, "success", "", time.Hour)
	runOnce(t, newScorer(pool, fastOptions()))
	assertNotApplied(t, s, d.ID)
}

// Invalid input, failed before: the index is gone because its table is
// gone; nobody applied the drop, so it is no evidence. Unscored at the
// horizon.
func TestScorerIndexDroppedWithItsTableIsNotExternal(t *testing.T) {
	pool, _ := testPool(t)
	shadowTable(t, pool, "shadow_gone_t")
	mustExec(t, pool, `CREATE INDEX shadow_gone_t_a ON public.shadow_gone_t (a)`)
	s := NewStore(pool)
	drop := sample("index_drop", `DROP INDEX CONCURRENTLY public.shadow_gone_t_a`)
	drop.RollbackSQL = `CREATE INDEX shadow_gone_t_a ON public.shadow_gone_t USING btree (a);`
	d := record(t, s, drop)
	age(t, pool, d.ID, 3*time.Hour)
	mustExec(t, pool, `DROP TABLE public.shadow_gone_t`)
	runOnce(t, newScorer(pool, fastOptions()))
	assertNotApplied(t, s, d.ID)
	unscoredAtHorizon(t, pool, s, d.ID)
}

// Empty input, failed before: a drop decision whose rollback names no
// table cannot tell a dropped index from a dropped table; it fails closed.
func TestScorerDropOfAnUnknownTableIsNotExternal(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	d := record(t, s, sample("index_drop", `DROP INDEX CONCURRENTLY public.shadow_never_idx`))
	age(t, pool, d.ID, 3*time.Hour)
	runOnce(t, newScorer(pool, fastOptions()))
	assertNotApplied(t, s, d.ID)
}

// Happy path: a migration drops the index of a live table.
func TestScorerExternalDropOnALiveTableIsDetected(t *testing.T) {
	pool, _ := testPool(t)
	shadowTable(t, pool, "shadow_live_t")
	mustExec(t, pool, `CREATE INDEX shadow_live_t_a ON public.shadow_live_t (a)`)
	s := NewStore(pool)
	drop := sample("index_drop", `DROP INDEX CONCURRENTLY public.shadow_live_t_a`)
	drop.RollbackSQL = `CREATE INDEX shadow_live_t_a ON public.shadow_live_t USING btree (a);`
	d := record(t, s, drop)
	age(t, pool, d.ID, 3*time.Hour)
	runOnce(t, newScorer(pool, fastOptions()))
	assertNotApplied(t, s, d.ID)
	mustExec(t, pool, `DROP INDEX public.shadow_live_t_a`)
	runOnce(t, newScorer(pool, fastOptions()))
	assertApplied(t, s, d.ID)
}

// Happy path: pg_sage's other index changes on the table (up to the
// lookup's limit) and its failed attempt at the same index do not hide a
// migration's index.
func TestScorerOtherOwnActionsKeepTheChangeExternal(t *testing.T) {
	pool, _ := testPool(t)
	shadowTable(t, pool, "shadow_many")
	s := NewStore(pool)
	d := record(t, s, sample("index_create",
		`CREATE INDEX CONCURRENTLY i_many ON public.shadow_many (a)`))
	age(t, pool, d.ID, 3*time.Hour)
	ownActions(t, pool, "create_index", "shadow_many", ownActionLimit, 2*time.Hour)
	executedAction(t, pool, "create_index",
		`CREATE INDEX CONCURRENTLY i_many ON public.shadow_many (a)`, "failed", "", time.Hour)
	// An earlier pg_sage index of the shape, before the decision, is not it.
	executedAction(t, pool, "create_index",
		`CREATE INDEX CONCURRENTLY i_many ON public.shadow_many (a)`, "success", "", 4*time.Hour)
	mustExec(t, pool, `CREATE INDEX shadow_many_mig ON public.shadow_many (a)`)
	runOnce(t, newScorer(pool, fastOptions()))
	assertApplied(t, s, d.ID)
}

// Boundary: more of pg_sage's index changes on the table than the lookup
// reads; it cannot rule out its own change and fails closed.
func TestScorerOwnActionLookupFailsClosedPastItsLimit(t *testing.T) {
	pool, _ := testPool(t)
	shadowTable(t, pool, "shadow_over")
	s := NewStore(pool)
	d := record(t, s, sample("index_create",
		`CREATE INDEX CONCURRENTLY i_over ON public.shadow_over (a)`))
	age(t, pool, d.ID, 3*time.Hour)
	ownActions(t, pool, "create_index", "shadow_over", ownActionLimit+1, 2*time.Hour)
	mustExec(t, pool, `CREATE INDEX shadow_over_mig ON public.shadow_over (a)`)
	runOnce(t, newScorer(pool, fastOptions()))
	assertNotApplied(t, s, d.ID)
}

// Error propagation: the read of pg_sage's own index changes fails
// (permission denied); the error names it and nothing is marked applied.
func TestScorerOwnActionReadErrorMarksNothing(t *testing.T) {
	pool, ctx := testPool(t)
	shadowTable(t, pool, "shadow_err")
	mustExec(t, pool, `CREATE INDEX shadow_err_mig ON public.shadow_err (a)`)
	s := NewStore(pool)
	d := record(t, s, sample("index_create",
		`CREATE INDEX CONCURRENTLY i_err ON public.shadow_err (a)`))
	mustExec(t, pool, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'shadow_no_ledger') THEN
			CREATE ROLE shadow_no_ledger;
		END IF; END $$`)
	cfg := pool.Config().Copy()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE shadow_no_ledger")
		return err
	}
	limited, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(limited.Close)
	ext, own, err := newScorer(limited, fastOptions()).external(ctx,
		pendingDecision{Decision: get(t, s, d.ID)})
	if err == nil || ext != nil || own ||
		!strings.Contains(err.Error(), "shadow: read own index actions") ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("external = %+v, %v; want the own-actions read error", ext, err)
	}
	assertNotApplied(t, s, d.ID)
}
