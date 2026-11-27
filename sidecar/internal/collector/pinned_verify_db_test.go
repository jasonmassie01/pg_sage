package collector

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/verify"
)

// collection-07, CHECK-02: an index whose target query drops out of the
// top-N after the index is created is still measured, through the open
// verification's pin (sage.verification, verify.OpenTargetIDs), and is
// retained on that evidence instead of reverted as unverifiable. The test
// lives here, not in verify, because it drives the collector's cycles;
// verify does not import the collector, so there is no cycle.

const pinnedTarget = "SELECT pg_sleep($1) AS pinned_target_marker"

// insertPinAction is a decision and its create_index action, as the
// executor records them before it opens a watch.
func insertPinAction(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var decisionID, actionID int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.decision
		(feature, intent, verdict, risk_tier, reason, evidence_id)
		VALUES ('verify-test', 'pinned target', 'execute', 'safe', 'test', $1)
		RETURNING id`, fmt.Sprintf("pin-%d", time.Now().UnixNano())).Scan(&decisionID)
	if err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, rollback_sql, decision_id)
		VALUES ('create_index', 'CREATE INDEX pin_idx ON text_w (v)',
		'DROP INDEX pin_idx', $1) RETURNING id`, decisionID).Scan(&actionID)
	if err != nil {
		t.Fatalf("insert action: %v", err)
	}
	return actionID
}

func runPinnedTarget(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sleep float64) {
	t.Helper()
	for range 10 {
		if _, err := pool.Exec(ctx, pinnedTarget, sleep); err != nil {
			t.Fatalf("target statement: %v", err)
		}
	}
}

// resetDatabaseStatements drops this database's pg_stat_statements rows
// (the bootstrap's), leaving the cluster's statistics epoch alone.
func resetDatabaseStatements(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, pool, `SELECT pg_stat_statements_reset(0::oid,
		(SELECT oid FROM pg_database WHERE datname = current_database()), 0::int8)`)
}

func pinWatchState(actionID, target int64, executed time.Time,
	window time.Duration) verify.WatchState {
	return verify.WatchState{
		ID: fmt.Sprintf("pin-watch-%d", actionID), ActionID: actionID,
		ExecutedAt: executed, Table: "public.text_w", IndexName: "public.pin_idx",
		Criterion: verify.Criterion{
			Kind: "per_query_latency", TargetIDs: []int64{target},
			MinGainPct: 20, RegressPct: 15, WriteImpactPct: 20,
			Window: window, HardMax: 2 * window,
		},
		Status: "pending", Window: window, NextEvaluationAt: executed.Add(window),
	}
}

func TestPinnedVerificationTarget_RetainedNotUnverifiable(t *testing.T) {
	ctx := context.Background()
	pool := textTestDB(t, ctx)
	bootstrapSage(t, ctx, pool)
	mustExec(t, pool, "CREATE INDEX pin_idx ON text_w (v)")
	resetDatabaseStatements(t, ctx, pool)
	store := verify.NewPostgresStateStore(pool, verify.DefaultMinSamples)
	c := unionCollector(t, pool, 10)
	c.WithPinSource(PinSourceFunc(store.OpenTargetIDs))
	window := time.Hour
	executed := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)

	// Before the index: the slow target leads the total ranking.
	for i := range 6 {
		runPinnedTarget(t, ctx, pool, 0.02)
		at := executed.Add(-55*time.Minute + time.Duration(i)*10*time.Minute)
		q, ok := queryWith(recordCycle(t, ctx, c, at), "pinned_target_marker")
		if !ok || !slices.Contains(q.AdmittedBy, "total") {
			t.Fatalf("pre-action cycle %d: target collected %v admitted %v; want by total",
				i, ok, q.AdmittedBy)
		}
	}
	target := queryIDOf(t, ctx, pool, "pinned_target_marker")
	actionID := insertPinAction(t, ctx, pool)
	if err := store.Create(ctx, pinWatchState(actionID, target, executed, window)); err != nil {
		t.Fatalf("open the verification: %v", err)
	}
	// After it: the index made the target fast, and other work pushed it
	// out of the top-N (20 - 2K - 1 pin = 17 rows by total time).
	runCompetitors(t, ctx, pool, 22, 20, "0.1")
	assertTargetOutOfTopN(t, ctx, c)
	for i := range 6 {
		runPinnedTarget(t, ctx, pool, 0.004)
		at := executed.Add(5*time.Minute + time.Duration(i)*10*time.Minute)
		q, ok := queryWith(recordCycle(t, ctx, c, at), "pinned_target_marker")
		if !ok || !slices.Equal(q.AdmittedBy, []string{"pinned"}) {
			t.Fatalf("post-action cycle %d: target collected %v admitted %v; want [pinned]",
				i, ok, q.AdmittedBy)
		}
	}
	assertRetained(t, ctx, pool, store, actionID, executed.Add(window+time.Minute))

	// The verification is complete: its target is no longer pinned.
	runPinnedTarget(t, ctx, pool, 0.004)
	if _, ok := queryWith(recordCycle(t, ctx, c, executed.Add(65*time.Minute)),
		"pinned_target_marker"); ok {
		t.Fatal("the target is still collected after its verification completed")
	}
}

// assertTargetOutOfTopN checks the control: the top-N by total time (the
// read before collection-07) no longer holds the target.
func assertTargetOutOfTopN(t *testing.T, ctx context.Context, c *Collector) {
	t.Helper()
	legacy, err := legacyQueries(ctx, c)
	if err != nil {
		t.Fatalf("legacy read: %v", err)
	}
	if _, ok := queryWith(legacy, "pinned_target_marker"); ok {
		t.Fatal("control: the target is still in the top-N by total; the test proves nothing")
	}
}

func assertRetained(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	store *verify.PostgresStateStore, actionID int64, now time.Time) {
	t.Helper()
	options := verify.DefaultOptions()
	options.InitialWindow, options.HardMax = time.Hour, 2*time.Hour
	options.Now = func() time.Time { return now }
	engine, err := verify.NewEngine(verify.NewPostgresObservationSource(pool), store, options)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	verdicts, err := engine.ResumeDue(ctx)
	if err != nil || len(verdicts) != 1 {
		t.Fatalf("resume: %d verdicts, %v; want 1", len(verdicts), err)
	}
	v := verdicts[0]
	if !v.Retain || v.Revert || v.Outcome != verify.OutcomeImproved {
		t.Fatalf("verdict %+v; want retained as improved", v)
	}
	var reason string
	var completed bool
	err = pool.QueryRow(ctx, `SELECT COALESCE(reason, ''), completed_at IS NOT NULL
		FROM sage.verification WHERE action_log_id = $1`, actionID).Scan(&reason, &completed)
	if err != nil || !completed || reason == "insufficient_samples" {
		t.Fatalf("sage.verification: reason %q completed %v (%v)", reason, completed, err)
	}
}
