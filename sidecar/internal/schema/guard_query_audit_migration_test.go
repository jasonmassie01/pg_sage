package schema

import (
	"strings"
	"testing"
)

// Spec §6.8, §7: one sage.guard_query_audit row per agent_query call, with
// the gate's reason and step and the pg_stat_statements snapshot the
// attribution view compares against (G1-10). Re-running bootstrap changes
// nothing.

func TestGuardQueryAuditMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool) // idempotent
	const pid = "agp_migtestmigtestmigtest"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.guard_query_audit WHERE principal_id = $1`,
			pid)
	})
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.guard_query_audit (database_id,
		principal_id, task_id, envelope_hash, verdict, reason, step, row_count, classes,
		fingerprint, broker_role, pss_dealloc, pss_reset)
		VALUES ('00000000-0000-4000-8000-0000000000a1', $1, 't', 'h', 'execute', NULL,
		NULL, 3, '{pii}', 'fp', 'sage_agentb_abcdefghij', 4, now()) RETURNING id`,
		pid).Scan(&id)
	if err != nil || id == 0 {
		t.Fatalf("insert: id %d, %v", id, err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.guard_query_audit (database_id, principal_id,
		envelope_hash, verdict) VALUES ('00000000-0000-4000-8000-0000000000a1', $1, 'h',
		'maybe')`, pid)
	if err == nil || !strings.Contains(err.Error(), "check") {
		t.Fatalf("verdict maybe accepted: %v", err)
	}
	var indexed bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes
		WHERE schemaname = 'sage' AND tablename = 'guard_query_audit'
		AND indexdef LIKE '%(principal_id, at%')`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Error("no (principal_id, at) index: the activity view would scan the table")
	}
}
