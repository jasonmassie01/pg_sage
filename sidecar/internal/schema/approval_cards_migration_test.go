package schema

import (
	"testing"
)

// Approval cards: the per-channel card deliveries (hashed token, expiry,
// single use, follow-up state) and the snooze columns of the action queue.
// The migration is idempotent and safe to re-run.

func TestApprovalCardsMigrationIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.approval_card_deliveries')
		IS NOT NULL`).Scan(&ok); err != nil || !ok {
		t.Fatalf("sage.approval_card_deliveries missing (%v)", err)
	}
	for table, cols := range map[string][]string{
		"approval_card_deliveries": {"token_sha256", "channel_id", "database_name",
			"queue_id", "card_hash", "title", "summary", "expires_at", "used_at",
			"used_by", "decision", "message_id", "followed_up_at", "followup_verdict"},
		"action_queue": {"snoozed_until", "snoozed_by", "snooze_reason"},
	} {
		for _, col := range cols {
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
				WHERE table_schema = 'sage' AND table_name = $1 AND column_name = $2`,
				table, col).Scan(&n); err != nil || n != 1 {
				t.Errorf("sage.%s.%s missing (%v)", table, col, err)
			}
		}
	}
	for _, idx := range []string{"idx_approval_card_followup", "idx_action_queue_snoozed"} {
		if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.' || $1) IS NOT NULL`,
			idx).Scan(&ok); err != nil || !ok {
			t.Errorf("index sage.%s missing (%v)", idx, err)
		}
	}
	// The token hash is exactly 32 bytes and a decision is one of three.
	if _, err := pool.Exec(ctx, `INSERT INTO sage.approval_card_deliveries
		(token_sha256, channel_id, database_name, queue_id, card_hash, expires_at)
		VALUES ('\x00'::bytea, 1, 'd', 1, 'h', now() + interval '1 hour')`); err == nil {
		t.Fatal("a short token hash was accepted")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.approval_card_deliveries
		(token_sha256, channel_id, database_name, queue_id, card_hash, expires_at, decision)
		VALUES (decode(repeat('ab', 32), 'hex'), 1, 'd', 1, 'h', now(), 'maybe')`); err == nil {
		t.Fatal("an unknown decision was accepted")
	}
}
