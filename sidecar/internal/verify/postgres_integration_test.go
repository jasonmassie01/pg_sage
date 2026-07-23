package verify

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresStateStorePersistsAndReloadsWatch(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	decisionID, actionID := insertVerificationAction(t, ctx, pool)
	t.Cleanup(func() { cleanupVerificationAction(pool, actionID, decisionID) })

	now := time.Now().UTC().Truncate(time.Microsecond)
	state := WatchState{
		ID:       fmt.Sprintf("integration-watch-%d", actionID),
		ActionID: actionID, ExecutedAt: now,
		Table: "public.orders", IndexName: "idx_orders",
		Criterion: Criterion{
			Kind: "per_query_latency", TargetIDs: []int64{91},
			Window: time.Minute, HardMax: time.Hour,
		},
		Status: "pending", Window: time.Minute,
		NextEvaluationAt: now.Add(time.Minute),
	}
	store := NewPostgresStateStore(pool, 30)
	if err := store.Create(ctx, state); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	got, err := store.Get(ctx, state.ID)
	if err != nil || got.ActionID != actionID || got.ID != state.ID {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	state.Status, state.Completed, state.Reason = "success", true, "gain_confirmed"
	if err := store.Update(ctx, state); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	got, err = store.Get(ctx, state.ID)
	if err != nil || !got.Completed || got.Status != "success" {
		t.Fatalf("completed Get() = %#v, %v", got, err)
	}
}

func TestPostgresObservationSourceReadsRealCollectorTables(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	from := time.Date(2099, 7, 22, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Minute)
	seedObservationRows(t, ctx, pool, from, to)
	t.Cleanup(func() { cleanupObservationRows(pool) })

	source := NewPostgresObservationSource(pool)
	queries, err := source.QueryMeasurements(ctx, []int64{990022}, from, to)
	if err != nil || queries[990022].Samples != 30 ||
		queries[990022].AverageLatency != 10*time.Millisecond {
		t.Fatalf("QueryMeasurements() = %#v, %v", queries, err)
	}
	writes, err := source.WriteMeasurements(ctx, "public.verify_adapter_test", from, to)
	if err != nil || writes.Samples != 2 || writes.AverageLatency != 30*time.Millisecond {
		t.Fatalf("WriteMeasurements() = %#v, %v", writes, err)
	}
	valid, err := source.IndexValid(ctx, "public.idx_verify_adapter_test")
	if err != nil || !valid {
		t.Fatalf("IndexValid() = %v, %v", valid, err)
	}
	if _, err := source.CurrentLoad(ctx); err != nil {
		t.Fatalf("CurrentLoad() error = %v", err)
	}
}

func verifyIntegrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SAGE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SKIPPED: SAGE_TEST_DATABASE_URL is not configured")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("test database unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func insertVerificationAction(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) (int64, int64) {
	t.Helper()
	var decisionID, actionID int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.decision
		(feature, intent, verdict, risk_tier, reason, evidence_id)
		VALUES ('verify-test', 'test durable watch', 'execute', 'safe',
		'test', $1) RETURNING id`,
		fmt.Sprintf("verify-integration-%d", time.Now().UnixNano())).Scan(&decisionID)
	if err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, rollback_sql, decision_id)
		VALUES ('create_index', 'SELECT 1', 'SELECT 1', $1) RETURNING id`,
		decisionID).Scan(&actionID)
	if err != nil {
		_, _ = pool.Exec(ctx, "DELETE FROM sage.decision WHERE id=$1", decisionID)
		t.Fatalf("insert action: %v", err)
	}
	return decisionID, actionID
}

func cleanupVerificationAction(pool *pgxpool.Pool, actionID, decisionID int64) {
	ctx := context.Background()
	_, _ = pool.Exec(ctx, "DELETE FROM sage.verification WHERE action_log_id=$1", actionID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.action_log WHERE id=$1", actionID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.decision WHERE id=$1", decisionID)
}

func seedObservationRows(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, from, to time.Time,
) {
	t.Helper()
	cleanupObservationRows(pool)
	mustExec(t, ctx, pool,
		"CREATE TABLE IF NOT EXISTS public.verify_adapter_test(id int)")
	mustExec(t, ctx, pool, `CREATE INDEX IF NOT EXISTS idx_verify_adapter_test
		ON public.verify_adapter_test(id)`)
	mustExec(t, ctx, pool, `INSERT INTO sage.query_store
		(captured_at, queryid, calls, total_exec_time, mean_exec_time)
		VALUES ($1, 990022, 10, 100, 10), ($2, 990022, 40, 400, 10)`, from, to)
	mustExec(t, ctx, pool, `INSERT INTO sage.snapshots(collected_at, category, data)
		VALUES ($1, 'system', '{"blk_write_time":100,"verify_fixture":true}'),
		($2, 'system', '{"blk_write_time":130,"verify_fixture":true}')`, from, to)
}

func mustExec(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any,
) {
	t.Helper()
	_, err := pool.Exec(ctx, sql, args...)
	if err != nil {
		t.Fatalf("fixture SQL failed: %v", err)
	}
}

func cleanupObservationRows(pool *pgxpool.Pool) {
	ctx := context.Background()
	_, _ = pool.Exec(ctx, "DELETE FROM sage.query_store WHERE queryid=990022")
	_, _ = pool.Exec(ctx, `DELETE FROM sage.snapshots
		WHERE category='system' AND data->>'verify_fixture'='true'`)
	_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS public.verify_adapter_test CASCADE")
}
