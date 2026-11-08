package schema

import "testing"

// policy-05: sage.action_queue.reverts_action_id names the action a revert
// item reverts. The migration is additive, idempotent and keeps the link
// consistent with sage.action_log.
func TestRevertQueueMigrationIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var dataType string
	if err := pool.QueryRow(ctx, `SELECT data_type FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'action_queue'
		  AND column_name = 'reverts_action_id'`).Scan(&dataType); err != nil {
		t.Fatalf("sage.action_queue.reverts_action_id missing: %v", err)
	}
	if dataType != "bigint" {
		t.Fatalf("reverts_action_id is %s, want bigint (sage.action_log.id)", dataType)
	}
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT
		to_regclass('sage.idx_action_queue_reverts_pending') IS NOT NULL`).
		Scan(&ok); err != nil || !ok {
		t.Fatalf("index sage.idx_action_queue_reverts_pending missing (%v)", err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.action_queue
		(proposed_sql, action_risk, reverts_action_id) VALUES ('SELECT 1', 'safe', -42)`)
	if err == nil {
		t.Fatal("a revert item naming no action was accepted")
	}
	var actionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (action_type, sql_executed)
		VALUES ('create_index', 'CREATE INDEX CONCURRENTLY rq ON public.t (a)')
		RETURNING id`).Scan(&actionID); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	var queueID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_queue
		(proposed_sql, action_risk, reverts_action_id)
		VALUES ('DROP INDEX CONCURRENTLY IF EXISTS public.rq', 'moderate', $1)
		RETURNING id`, actionID).Scan(&queueID); err != nil {
		t.Fatalf("insert revert item: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.action_log WHERE id = $1`,
		actionID); err != nil {
		t.Fatalf("delete reverted action: %v", err)
	}
	var link *int64
	if err := pool.QueryRow(ctx, `SELECT reverts_action_id FROM sage.action_queue
		WHERE id = $1`, queueID).Scan(&link); err != nil || link != nil {
		t.Fatalf("revert link after the action was deleted = %v (%v), want NULL", link, err)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM sage.action_queue WHERE id = $1`, queueID)
}
