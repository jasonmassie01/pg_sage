package verify

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// executor-02 on real PostgreSQL: the observation-failure count is durable,
// the startup sweep reopens rows that a transient error closed before the
// fix, and a metadata-database outage under history.store: meta keeps the
// watch open instead of reverting it.

func createDBWatch(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, executedAt time.Time,
) (*PostgresStateStore, WatchState) {
	t.Helper()
	decisionID, actionID := insertVerificationAction(t, ctx, pool)
	t.Cleanup(func() { cleanupVerificationAction(pool, actionID, decisionID) })
	state := WatchState{
		ID: fmt.Sprintf("retry-watch-%d", actionID), ActionID: actionID,
		ExecutedAt: executedAt, Table: "public.orders", IndexName: "public.idx_orders",
		Criterion: Criterion{
			Kind: "per_query_latency", TargetIDs: []int64{91},
			Window: time.Hour, HardMax: 72 * time.Hour,
		},
		Status: "pending", Window: time.Hour, NextEvaluationAt: executedAt.Add(time.Hour),
	}
	store := NewPostgresStateStore(pool, 30)
	if err := store.Create(ctx, state); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return store, state
}

func TestPostgresStateStorePersistsObservationFailures(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Microsecond)
	store, state := createDBWatch(t, ctx, pool, now.Add(-2*time.Hour))
	got, err := store.Get(ctx, state.ID)
	if err != nil || got.ObservationFailures != 0 {
		t.Fatalf("created watch = %#v, %v; want 0 failures", got, err)
	}
	state.Status, state.Reason = "extended", "observation_retry: connection refused"
	state.ObservationFailures, state.NextEvaluationAt = 3, now.Add(-time.Second)
	if err := store.Update(ctx, state); err != nil {
		t.Fatalf("Update: %v", err)
	}
	due, err := store.ListDue(ctx, now)
	if err != nil {
		t.Fatalf("ListDue: %v", err)
	}
	var listed *WatchState
	for i := range due {
		if due[i].ActionID == state.ActionID {
			listed = &due[i]
		}
	}
	if listed == nil || listed.ObservationFailures != 3 {
		t.Fatalf("listed watch = %#v, want 3 failures", listed)
	}
	claimed, ok, err := store.Claim(ctx, *listed, now, now.Add(RevertRetryInterval))
	if err != nil || !ok || claimed.ObservationFailures != 3 ||
		claimed.Reason != state.Reason {
		t.Fatalf("Claim = %#v, %v, %v; want the retry row with 3 failures", claimed, ok, err)
	}
}

func TestReopenObservationRetriesSweepsOnlyTransientPreFixRows(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	now := time.Now().UTC()
	rows := map[string]sweepRow{
		"observation_unavailable ahead": {"observation_unavailable", time.Hour, false, true},
		"context_canceled ahead":        {"context_canceled", time.Hour, false, true},
		"observation_unavailable past":  {"observation_unavailable", -time.Hour, false, false},
		"insufficient_samples ahead":    {"insufficient_samples", time.Hour, false, false},
		"completed":                     {"observation_unavailable", time.Hour, true, false},
	}
	ids := map[string]int64{}
	var store *PostgresStateStore
	for name, r := range rows {
		var state WatchState
		store, state = createDBWatch(t, ctx, pool, now.Add(-3*time.Hour))
		mustExec(t, ctx, pool, `UPDATE sage.verification SET verdict='unverifiable',
			reason=$2, hard_deadline_at=now()+$3::interval,
			next_evaluation_at=now()-interval '1 minute',
			completed_at=CASE WHEN $4 THEN now() END WHERE action_log_id=$1`,
			state.ActionID, r.reason, fmt.Sprintf("%d seconds", int(r.deadline.Seconds())),
			r.completed)
		ids[name] = state.ActionID
	}
	reopened, err := store.ReopenObservationRetries(ctx)
	if err != nil || reopened < 2 {
		t.Fatalf("ReopenObservationRetries = %d, %v; want at least the 2 fixture rows",
			reopened, err)
	}
	assertSweptRows(t, ctx, pool, rows, ids)
	again, err := store.ReopenObservationRetries(ctx)
	if err != nil || again != 0 {
		t.Fatalf("second sweep = %d, %v; want 0 (idempotent)", again, err)
	}
}

func TestReopenedPreFixRowIsObservedNotReverted(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	source := newFakeObservationSource()
	executedAt := source.executedAt
	store, state := createDBWatch(t, ctx, pool, executedAt)
	mustExec(t, ctx, pool, `UPDATE sage.verification SET verdict='unverifiable',
		reason='observation_unavailable', hard_deadline_at=now()+interval '1 day',
		next_evaluation_at=now()-interval '1 minute' WHERE action_log_id=$1`, state.ActionID)
	if _, err := store.ReopenObservationRetries(ctx); err != nil {
		t.Fatalf("ReopenObservationRetries: %v", err)
	}
	options := DefaultOptions()
	options.Now = time.Now
	engine, err := NewEngine(source, store, options)
	if err != nil {
		t.Fatal(err)
	}
	results, err := engine.ResumeDueResults(ctx)
	if err != nil {
		t.Fatalf("ResumeDueResults: %v", err)
	}
	for _, result := range results {
		if result.ActionID != state.ActionID {
			continue
		}
		if result.Verdict.Revert || source.queries() == 0 {
			t.Fatalf("reopened row verdict = %#v after %d reads; want an observation",
				result.Verdict, source.queries())
		}
		return
	}
	t.Fatalf("reopened row was not resumed: %#v", results)
}

func TestMetaHistoryOutageKeepsWatchOpen(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	p.Switch(t, histstore.ModeMeta)
	mustExec(t, ctx, p.Monitored, `CREATE TABLE IF NOT EXISTS public.orders (a int);
		CREATE INDEX IF NOT EXISTS idx_orders ON public.orders (a)`)
	now := time.Now().UTC().Truncate(time.Microsecond)
	decisionID, actionID := insertVerificationAction(t, ctx, p.Monitored)
	t.Cleanup(func() { cleanupVerificationAction(p.Monitored, actionID, decisionID) })
	source := NewPostgresObservationSource(p.Monitored)
	store := NewPostgresStateStore(p.Monitored, 30)
	engine, err := NewEngine(source, store, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	p.Meta.Close() // the metadata database goes away mid-watch
	verdict, err := engine.Watch(ctx, WatchRequest{
		ID: fmt.Sprintf("meta-outage-%d", actionID), ActionID: actionID,
		ExecutedAt: now.Add(-2 * time.Hour), Table: "public.orders",
		IndexName: "public.idx_orders",
		Criterion: Criterion{Kind: "per_query_latency", TargetIDs: []int64{77}},
	})
	if err != nil || verdict.Revert || verdict.Status != "extended" {
		t.Fatalf("Watch during meta outage = %#v, %v; want an open retry", verdict, err)
	}
	var status, reason string
	var failures, present int
	if err := p.Monitored.QueryRow(ctx, `SELECT verdict, reason, observation_failures,
		(SELECT count(*) FROM pg_index WHERE indexrelid=to_regclass('public.idx_orders'))
		FROM sage.verification WHERE action_log_id=$1`, actionID).
		Scan(&status, &reason, &failures, &present); err != nil {
		t.Fatalf("read watch row: %v", err)
	}
	if status != "extended" || !strings.HasPrefix(reason, "observation_retry: ") ||
		failures != 1 || present != 1 {
		t.Fatalf("watch row = %s/%q/%d failures, index present %d", status, reason,
			failures, present)
	}
}

type sweepRow struct {
	reason     string
	deadline   time.Duration
	completed  bool
	wantReopen bool
}

func assertSweptRows(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	rows map[string]sweepRow, ids map[string]int64,
) {
	t.Helper()
	for name, r := range rows {
		var verdict, reason string
		if err := pool.QueryRow(ctx, `SELECT verdict, reason FROM sage.verification
			WHERE action_log_id=$1`, ids[name]).Scan(&verdict, &reason); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		reopenedRow := verdict == "extended" && strings.HasPrefix(reason, "observation_retry: ")
		if reopenedRow != r.wantReopen {
			t.Errorf("%s: verdict=%s reason=%q, reopened=%v want %v",
				name, verdict, reason, reopenedRow, r.wantReopen)
		}
	}
}
