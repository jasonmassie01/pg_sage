package schema

import (
	"strings"
	"testing"
)

// One change per object: the in-flight lookup reads the pending verdicts
// of sage.action_outcome through a partial index, so it never scans the
// outcome ledger. The migration is idempotent.

func TestVerificationWaitMigration_CreatesThePendingIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	var before uint32
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
		var oid uint32
		if err := pool.QueryRow(ctx, `SELECT COALESCE(to_regclass(
			'sage.idx_action_outcome_pending')::oid, 0)`).Scan(&oid); err != nil {
			t.Fatal(err)
		}
		if oid == 0 {
			t.Fatal("idx_action_outcome_pending missing")
		}
		if run == 1 && oid != before {
			t.Fatalf("a re-run rebuilt the index (oid %d -> %d)", before, oid)
		}
		before = oid
	}
	var def string
	if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(
		'sage.idx_action_outcome_pending'::regclass)`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"sage.action_outcome", "(action_log_id)",
		"WHERE (verdict = 'pending'::text)"} {
		if !strings.Contains(def, part) {
			t.Fatalf("index %s lacks %s", def, part)
		}
	}
}
