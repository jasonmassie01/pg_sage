package retention

import (
	"slices"
	"testing"
)

// Perf storage phase: the action queue had no retention at all. Terminal
// rows age out; rows that back an action or a decision are kept.

// The approval queue keeps pending work, rows an action, a decision or an
// SRE proposal still points at; decided rows age out with actions_days.
func TestRunOnce_ActionQueueTerminalRowsAgeOut(t *testing.T) {
	pool, ctx := requireDB(t)
	tag := uniqueTag("queue")
	insert := func(status, extra string) int64 {
		return insertID(t, ctx, `INSERT INTO sage.action_queue (proposed_sql, action_risk,
			status, proposed_at, decided_at, expires_at, reason)
			VALUES ('SELECT 1', 'safe', $1, now() - interval '400 days',
			        now() - interval '400 days', now() - interval '393 days'
			        `+extra+`, $2) RETURNING id`, status, tag)
	}
	pending := insert("pending", "")
	rejected := insert("rejected", "")
	expired := insert("expired", "")
	failedRetrying := insertID(t, ctx, `INSERT INTO sage.action_queue (proposed_sql,
		action_risk, status, proposed_at, expires_at, reason) VALUES ('SELECT 1', 'safe',
		'failed', now() - interval '400 days', now() + interval '1 day', $1) RETURNING id`, tag)
	logID := insertID(t, ctx, `INSERT INTO sage.action_log (action_type, sql_executed,
		outcome, executed_at) VALUES ('t', 'SELECT 1', 'success', now()) RETURNING id`)
	executedLive := insertID(t, ctx, `INSERT INTO sage.action_queue (proposed_sql, action_risk,
		status, proposed_at, decided_at, action_log_id, reason) VALUES ('SELECT 1', 'safe',
		'executed', now() - interval '400 days', now() - interval '400 days', $1, $2)
		RETURNING id`, logID, tag)
	byDecision := insert("rejected", "")
	execRetry(t, ctx, `INSERT INTO sage.decision (feature, intent, verdict, risk_tier, reason,
		evidence_id, queue_id) VALUES ('t', 'i', 'queue_approval', 'safe', 'r', $1, $2)`,
		tag, byDecision)
	New(pool, allDays(365), noopLog).RunOnce(ctx)
	rows, err := pool.Query(ctx, `SELECT id FROM sage.action_queue WHERE reason = $1
		ORDER BY id`, tag)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kept []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, id)
	}
	want := []int64{pending, failedRetrying, executedLive, byDecision}
	slices.Sort(want)
	if !slices.Equal(kept, want) {
		t.Fatalf("kept %v, want %v (rejected %d and expired %d purged)", kept, want,
			rejected, expired)
	}
	execRetry(t, ctx, `DELETE FROM sage.decision WHERE evidence_id = $1`, tag)
	execRetry(t, ctx, `DELETE FROM sage.action_queue WHERE reason = $1`, tag)
	execRetry(t, ctx, `DELETE FROM sage.action_log WHERE id = $1`, logID)
}
