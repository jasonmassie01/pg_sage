package verify

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// executor-03 on real PostgreSQL: an index built on table A at night, then a
// daytime batch that writes table B. The collector's database-wide
// blk_write_time readings rise 50% after the build, but nothing charges
// that to the index: the queries the index serves gained 30%, so it stays.

const writeSignalQueryID = int64(990_301)

func cleanupWriteSignalFixture(pool *pgxpool.Pool) {
	ctx := context.Background()
	_, _ = pool.Exec(ctx, "DELETE FROM sage.query_store WHERE queryid=$1", writeSignalQueryID)
	_, _ = pool.Exec(ctx, `DELETE FROM sage.snapshots
		WHERE category='system' AND data->>'write_signal_fixture'='true'`)
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS public.verify_wsig_a, public.verify_wsig_b")
}

// seedWriteSignalHistory writes the collector history around executedAt:
// six 10-minute query intervals of 20 calls at about 100 ms before the
// build and about 70 ms after, and system snapshots whose blk_write_time
// grows 100 ms per interval before and 150 ms per interval after.
func seedWriteSignalHistory(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, executedAt time.Time,
) {
	t.Helper()
	before := []float64{2000, 2100, 1900, 2000, 2050, 1950}
	after := []float64{1400, 1450, 1350, 1400, 1420, 1380}
	at := executedAt.Add(-61 * time.Minute)
	calls, total := int64(1000), 50000.0
	insert := func() {
		mustExec(t, ctx, pool, `INSERT INTO sage.query_store
			(captured_at, queryid, calls, total_exec_time, mean_exec_time)
			VALUES ($1, $2, $3, $4, 0)`, at, writeSignalQueryID, calls, total)
	}
	insert()
	for _, delta := range append(before, after...) {
		at, calls, total = at.Add(10*time.Minute), calls+20, total+delta
		insert()
	}
	writes := map[time.Duration]int{-50: 1000, -30: 1100, -10: 1200, 10: 5000, 30: 5150,
		50: 5300}
	for offset, ms := range writes {
		mustExec(t, ctx, pool, `INSERT INTO sage.snapshots(collected_at, category, data)
			VALUES ($1, 'system', jsonb_build_object('blk_write_time', $2::int,
			'write_signal_fixture', true))`, executedAt.Add(offset*time.Minute), ms)
	}
}

func TestNightBuiltIndexSurvivesDaytimeWriteSurgeOnAnotherTable(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	cleanupWriteSignalFixture(pool)
	t.Cleanup(func() { cleanupWriteSignalFixture(pool) })
	mustExec(t, ctx, pool, `CREATE TABLE public.verify_wsig_a AS
		SELECT g AS id, g % 97 AS customer FROM generate_series(1, 5000) g`)
	mustExec(t, ctx, pool, "CREATE TABLE public.verify_wsig_b(id int, payload text)")
	mustExec(t, ctx, pool,
		"CREATE INDEX idx_verify_wsig_a ON public.verify_wsig_a(customer)")
	mustExec(t, ctx, pool, `INSERT INTO public.verify_wsig_b
		SELECT g, repeat('x', 200) FROM generate_series(1, 20000) g`)

	executedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	seedWriteSignalHistory(t, ctx, pool, executedAt)
	decisionID, actionID := insertVerificationAction(t, ctx, pool)
	t.Cleanup(func() { cleanupVerificationAction(pool, actionID, decisionID) })

	options := DefaultOptions()
	options.Now = func() time.Time { return executedAt.Add(61 * time.Minute) }
	engine, err := NewEngine(NewPostgresObservationSource(pool),
		NewPostgresStateStore(pool, options.MinSamples), options)
	if err != nil {
		t.Fatal(err)
	}
	verdict, err := engine.Watch(ctx, WatchRequest{
		ID: "write-signal-night-build", ActionID: actionID, ExecutedAt: executedAt,
		Table: "public.verify_wsig_a", IndexName: "public.idx_verify_wsig_a",
		Criterion: Criterion{Kind: "per_query_latency",
			TargetIDs: []int64{writeSignalQueryID}, MinGainPct: 20, RegressPct: 15,
			WriteImpactPct: 20, Window: time.Hour, HardMax: 72 * time.Hour},
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if !verdict.Retain || verdict.Revert || verdict.Status != "success" {
		t.Fatalf("verdict = %#v, want the night-built index retained", verdict)
	}
	signal := writeSignalOf(t, verdict)
	if signal["exceeds_threshold"] != true {
		t.Fatalf("write_signal = %#v, want the 50%% surge over the threshold", signal)
	}
	assertSignalFloat(t, signal, "before_ms", 100)
	assertSignalFloat(t, signal, "after_ms", 150)
	assertStoredVerdict(t, ctx, pool, actionID, "success")
}

func assertStoredVerdict(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, actionID int64, want string,
) {
	t.Helper()
	var verdict string
	var completed bool
	err := pool.QueryRow(ctx, `SELECT verdict, completed_at IS NOT NULL
		FROM sage.verification WHERE action_log_id=$1`, actionID).Scan(&verdict, &completed)
	if err != nil || verdict != want || !completed {
		t.Fatalf("stored verification = %q completed=%v, %v; want completed %q",
			verdict, completed, err, want)
	}
}

// A write_impact revert stored before the upgrade, whose drop has not run
// yet (withheld or failed), is reopened by the startup sweep and observed
// again instead of dropping the index on the next resume.
func TestReopenSweepReopensPendingWriteImpactReverts(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	now := time.Now().UTC()
	store, pending := createDBWatch(t, ctx, pool, now.Add(-3*time.Hour))
	_, done := createDBWatch(t, ctx, pool, now.Add(-3*time.Hour))
	_, regressed := createDBWatch(t, ctx, pool, now.Add(-3*time.Hour))
	for _, row := range []struct {
		id        int64
		reason    string
		completed bool
	}{{pending.ActionID, "write_impact", false}, {done.ActionID, "write_impact", true},
		{regressed.ActionID, "query_regression", false}} {
		mustExec(t, ctx, pool, `UPDATE sage.verification SET verdict='revert', reason=$2,
			hard_deadline_at=now()-interval '1 hour',
			completed_at=CASE WHEN $3 THEN now() END WHERE action_log_id=$1`,
			row.id, row.reason, row.completed)
	}
	reopened, err := store.ReopenObservationRetries(ctx)
	if err != nil || reopened < 1 {
		t.Fatalf("ReopenObservationRetries = %d, %v; want the pending write_impact row",
			reopened, err)
	}
	for id, want := range map[int64]string{pending.ActionID: "extended",
		done.ActionID: "revert", regressed.ActionID: "revert"} {
		var verdict, reason string
		err := pool.QueryRow(ctx, `SELECT verdict, reason FROM sage.verification
			WHERE action_log_id=$1`, id).Scan(&verdict, &reason)
		if err != nil || verdict != want {
			t.Fatalf("action %d verdict = %q (%q), %v; want %q", id, verdict, reason, err, want)
		}
		if want == "extended" && reason != "observation_retry: reopened write_impact verdict" {
			t.Fatalf("reopened reason = %q", reason)
		}
	}
	if again, err := store.ReopenObservationRetries(ctx); err != nil || again != 0 {
		t.Fatalf("second sweep = %d, %v; want 0 (idempotent)", again, err)
	}
}
