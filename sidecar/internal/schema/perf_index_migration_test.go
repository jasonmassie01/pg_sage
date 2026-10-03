package schema

import (
	"strings"
	"testing"
)

// Performance gate (reviews/2026-10-03-perf-gate-report.md): the retention
// purges and the foreign-key actions they fire must find their rows
// through an index, or every purge pass reads whole history tables.
func TestPerfIndexMigration_CreatesIdempotentIndexes(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	want := map[string]string{
		"idx_explain_cache_captured":      "(captured_at)",
		"idx_alert_log_sent":              "(sent_at)",
		"idx_verification_created":        "(created_at)",
		"idx_verification_decision":       "(decision_id)",
		"idx_sre_change_events_received":  "(deployment_id, received_at)",
		"idx_findings_action_log":         "(action_log_id) WHERE (action_log_id IS NOT NULL)",
		"idx_decision_action_log":         "(action_log_id) WHERE (action_log_id IS NOT NULL)",
		"idx_decision_queue":              "(queue_id) WHERE (queue_id IS NOT NULL)",
		"idx_action_queue_action_log":     "(action_log_id) WHERE (action_log_id IS NOT NULL)",
		"idx_change_lease_decision":       "(decision_id)",
		"idx_incident_avoided_action_log": "(action_log_id)",
		"idx_incident_avoided_decision":   "(decision_id)",
		"idx_incident_avoided_verify":     "(verification_id)",
		"idx_schema_baseline_action": "(last_authorized_action_id) " +
			"WHERE (last_authorized_action_id IS NOT NULL)",
		"idx_schema_baseline_decision": "(last_authorized_decision_id) " +
			"WHERE (last_authorized_decision_id IS NOT NULL)",
		"idx_verification_open_due": "(next_evaluation_at) WHERE (completed_at IS NULL)",
		"idx_decision_created":      "(created_at)",
	}
	for name, suffix := range want {
		var def string
		err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes
			WHERE schemaname = 'sage' AND indexname = $1`, name).Scan(&def)
		if err != nil {
			t.Errorf("index %s missing: %v", name, err)
			continue
		}
		if !strings.HasSuffix(def, suffix) {
			t.Errorf("index %s = %s, want suffix %s", name, def, suffix)
		}
	}
}

// Every retention purge predicate the gate found scanning sequentially is
// index-servable now (sequential scans disabled, the plan must not fall
// back to one).
func TestPerfIndexMigration_PurgePredicatesUseIndexes(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatal(err)
	}
	purges := map[string]string{
		"explain_cache": `SELECT ctid FROM sage.explain_cache
			WHERE captured_at < now() - make_interval(days => 90) LIMIT 1000`,
		"alert_log": `SELECT ctid FROM sage.alert_log
			WHERE sent_at < now() - make_interval(days => 365) LIMIT 1000`,
		"verification": `SELECT ctid FROM sage.verification
			WHERE created_at < now() - make_interval(days => 365)
			AND verdict NOT IN ('pending', 'extended') LIMIT 1000`,
		"findings": `SELECT ctid FROM sage.findings
			WHERE resolved_at < now() - make_interval(days => 180)
			AND status = 'resolved' LIMIT 1000`,
		"sre_change_events": `SELECT id FROM sage.sre_change_events
			WHERE deployment_id = gen_random_uuid() AND received_at < now()`,
		"verification by decision": `SELECT 1 FROM sage.verification WHERE decision_id = 7`,
		"verification due": `SELECT action_log_id FROM sage.verification
			WHERE completed_at IS NULL
			  AND verdict IN ('pending', 'extended', 'revert', 'unverifiable')
			  AND next_evaluation_at <= now() ORDER BY next_evaluation_at, id`,
		"decision": `SELECT ctid FROM sage.decision
			WHERE created_at < now() - make_interval(days => 365) LIMIT 1000`,
	}
	for name, sql := range purges {
		var plan string
		if err := conn.QueryRow(ctx, "EXPLAIN (FORMAT TEXT) "+sql).Scan(&plan); err != nil {
			t.Fatalf("%s: explain: %v", name, err)
		}
		if strings.Contains(plan, "Seq Scan") {
			t.Errorf("%s still plans a sequential scan:\n%s", name, plan)
		}
	}
}

// unindexedFKExemptions are foreign keys whose parent rows are
// configuration that pg_sage never purges, so no delete ever fires the
// key's action. Every other sage foreign key must have an index whose
// leading columns are the key's columns.
var unindexedFKExemptions = map[string]string{
	"config(updated_by_user_id)":     "users are deleted by an admin, rarely; config is small",
	"config_audit(changed_by)":       "users are deleted by an admin, rarely",
	"chatops_identities(user_id)":    "users are deleted by an admin, rarely; tiny table",
	"notification_rules(channel_id)": "notification channels are configuration; tiny table",
	"policy(supersedes_id)":          "policy versions are kept, never deleted",
	"decision(policy_id)":            "policy versions are kept, never deleted",
	"sre_runbook_runs(deployment_id, database_id, runbook_id, version)": "runbook " +
		"versions are immutable and never deleted",
	// Investigation-scoped keys: the primary key prefix (deployment_id,
	// database_id, investigation_id) bounds the check to one
	// investigation's rows, never the whole table.
	"sre_budget_reservations(deployment_id, database_id, investigation_id)": "bounded " +
		"by sre_budget_request (deployment_id, database_id, investigation_id, ...)",
	"sre_evidence(deployment_id, database_id, investigation_id, step_id)": "bounded " +
		"by the primary key prefix (deployment_id, database_id, investigation_id)",
}

// TestSageForeignKeysAreIndexed is a permanent guard: a new foreign key on
// a purged parent without an index makes every purge of that parent scan
// the child table once per deleted row.
func TestSageForeignKeysAreIndexed(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	rows, err := pool.Query(ctx, `SELECT c.conrelid::regclass::text, (
	        SELECT string_agg(a.attname, ', ' ORDER BY k.ord)
	        FROM unnest(c.conkey) WITH ORDINALITY k(attnum, ord)
	        JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum)
	    FROM pg_constraint c
	    JOIN pg_namespace n ON n.oid = c.connamespace
	    WHERE c.contype = 'f' AND n.nspname = 'sage'
	      AND NOT EXISTS (
	        SELECT 1 FROM pg_index i
	        WHERE i.indrelid = c.conrelid
	          AND (i.indkey::int2[])[0:cardinality(c.conkey) - 1] @> c.conkey
	          AND (i.indkey::int2[])[0:cardinality(c.conkey) - 1] <@ c.conkey)
	    ORDER BY 1, 2`)
	if err != nil {
		t.Fatalf("list unindexed foreign keys: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, cols string
		if err := rows.Scan(&table, &cols); err != nil {
			t.Fatal(err)
		}
		key := strings.TrimPrefix(table, "sage.") + "(" + cols + ")"
		if _, ok := unindexedFKExemptions[key]; !ok {
			t.Errorf("foreign key %s has no index on its columns", key)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
