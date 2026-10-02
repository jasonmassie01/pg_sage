package schema

import (
	"context"
	"strings"
	"testing"
)

// CHECK-28 (Sage SRE M1): the SRE coordination migration is additive and
// idempotent, and an upgrade preserves existing incidents and actions.

// Parents before children; the drop runs in reverse. The M2 tables
// (hypotheses, events, tombstones) reference investigations and the M5
// SLO tables reference the database bindings, so a pre-M1 simulation
// must drop them first.
var sreTables = []string{"sre_deployments", "sre_database_bindings",
	"sre_investigations", "sre_steps", "sre_evidence", "sre_budget_reservations",
	"sre_hypotheses", "sre_events", "sre_tombstones", "sre_service_slos",
	"sre_slo_transitions"}

func TestSREMigration_IdempotentAndPreservesIncidentsAndActions(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	// Simulate a pre-M1 install: no SRE tables.
	for i := len(sreTables) - 1; i >= 0; i-- {
		if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS sage."+sreTables[i]); err != nil {
			t.Fatalf("drop %s: %v", sreTables[i], err)
		}
	}
	var incidentID string
	var actionID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, causal_chain, signal_ids, source, confidence, database_name)
		VALUES ('warning', 'sre-m1 upgrade fixture', '[]'::jsonb,
		ARRAY['lock_contention'], 'deterministic', 1.0, 'sre_m1_db')
		RETURNING id::text`).Scan(&incidentID); err != nil {
		t.Fatalf("insert incident: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome) VALUES ('sre_m1_fixture', 'SELECT 1', 'success')
		RETURNING id`).Scan(&actionID); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE id=$1::uuid", incidentID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id=$1", actionID)
	})

	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, tbl := range sreTables {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema='sage' AND table_name=$1`, tbl).Scan(&n); err != nil || n != 1 {
			t.Fatalf("sage.%s after two upgrades: count=%d err=%v", tbl, n, err)
		}
	}
	var rootCause, outcome string
	if err := pool.QueryRow(ctx, "SELECT root_cause FROM sage.incidents WHERE id=$1::uuid",
		incidentID).Scan(&rootCause); err != nil || rootCause != "sre-m1 upgrade fixture" {
		t.Fatalf("incident lost on upgrade: %q %v", rootCause, err)
	}
	if err := pool.QueryRow(ctx, "SELECT outcome FROM sage.action_log WHERE id=$1",
		actionID).Scan(&outcome); err != nil || outcome != "success" {
		t.Fatalf("action lost on upgrade: %q %v", outcome, err)
	}
}

// The hard ceilings and the one-live-trigger rule are enforced by the
// database, not only by Go.
func TestSREMigration_ConstraintsEnforceCeilings(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	var idx int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname='sage'
		AND indexname='sre_one_live_trigger'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("sre_one_live_trigger index = %d (%v)", idx, err)
	}
	var checks string
	if err := pool.QueryRow(ctx, `SELECT string_agg(pg_get_constraintdef(oid), ' | ')
		FROM pg_constraint WHERE conrelid = 'sage.sre_investigations'::regclass
		AND contype = 'c'`).Scan(&checks); err != nil {
		t.Fatalf("read checks: %v", err)
	}
	for _, want := range []string{"120000", "12", "'queued'", "'needs_evidence'"} {
		if !strings.Contains(checks, want) {
			t.Fatalf("sre_investigations checks lack %s: %s", want, checks)
		}
	}
}
