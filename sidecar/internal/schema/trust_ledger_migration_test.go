package schema

import (
	"context"
	"strings"
	"testing"
)

// Roadmap 1.2 (one trust system): the ledger's outcomes keep the raw
// verdict, when it was observed in the monitored database and, for an
// operator rejection, the queue item; results gain "rejected" and sources
// "rollback"; levels gain the "grandfathered" provenance and the history
// its event; sage.trust_ledger_state holds each database's grandfathering
// marker and reconcile cursors. Additive and idempotent.

const trustDeployment = "72727272-7272-4272-8272-727272727272"

func trustCleanup(t *testing.T) {
	t.Helper()
	pool, _ := requireDB(t)
	clean := func() {
		ctx := context.Background()
		for _, table := range []string{"sre_autonomy_outcomes", "sre_family_autonomy",
			"trust_ledger_state"} {
			_, _ = pool.Exec(ctx, "DELETE FROM sage."+table+" WHERE deployment_id = $1",
				trustDeployment)
		}
	}
	clean()
	t.Cleanup(clean)
}

func TestTrustLedgerMigrationIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 3; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, column := range []string{"verdict", "observed_at", "queue_id"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = 'sage' AND table_name = 'sre_autonomy_outcomes'
			  AND column_name = $1`, column).Scan(&n); err != nil || n != 1 {
			t.Fatalf("column %s: %d (%v)", column, n, err)
		}
	}
	var state int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'trust_ledger_state'`).Scan(&state)
	if err != nil || state < 6 {
		t.Fatalf("trust_ledger_state columns = %d (%v)", state, err)
	}
}

func TestTrustLedgerMigrationAcceptsTheNewValues(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	trustCleanup(t)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_outcomes
		(deployment_id, database_name, queue_id, family, action_class, level, result,
		 source, actor, verdict, observed_at)
		VALUES ($1, 'orders', 41, 'tuning', 'index_create', 2, 'rejected', 'operator',
		        'user:7', 'rejected', now())`, trustDeployment); err != nil {
		t.Fatalf("rejection outcome: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_outcomes
		(deployment_id, database_name, action_log_id, family, action_class, level, result,
		 source, actor, verdict, observed_at)
		VALUES ($1, 'orders', 42, 'tuning', 'config_guc', 3, 'rejected', 'rollback',
		        'pg_sage', 'rolled_back', now())`, trustDeployment); err != nil {
		t.Fatalf("rollback outcome: %v", err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_outcomes
		(deployment_id, database_name, queue_id, family, action_class, level, result,
		 source, actor) VALUES ($1, 'orders', 41, 'tuning', 'index_create', 2, 'rejected',
		 'operator', 'user:7')`, trustDeployment)
	if err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("a second record of the same rejection: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_family_autonomy
		(deployment_id, database_name, family, action_class, level, changed_by,
		 change_reason, provenance, carried_ref)
		VALUES ($1, 'orders', 'hygiene', 'vacuum', 3, 'pg_sage', 'grandfathered',
		        'grandfathered', 'trust.level=autonomous, ramp elapsed')`,
		trustDeployment); err != nil {
		t.Fatalf("grandfathered level: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_autonomy_events
		(deployment_id, family, action_class, event_type, from_level, to_level, actor,
		 reason, database_name)
		VALUES ($1, 'hygiene', 'vacuum', 'grandfathered', 1, 3, 'pg_sage', 'kept', 'orders')`,
		trustDeployment); err != nil {
		t.Fatalf("grandfathered event: %v", err)
	}
}

func TestTrustLedgerMigrationKeepsTheChecks(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	trustCleanup(t)
	bad := []string{
		`INSERT INTO sage.sre_autonomy_outcomes (deployment_id, database_name, family,
		 action_class, level, result, source, actor) VALUES ($1, 'orders', 'tuning',
		 'index_create', 2, 'great', 'operator', 'x')`,
		`INSERT INTO sage.sre_autonomy_outcomes (deployment_id, database_name, family,
		 action_class, level, result, source, actor) VALUES ($1, 'orders', 'tuning',
		 'index_create', 2, 'rejected', 'cron', 'x')`,
		`INSERT INTO sage.sre_family_autonomy (deployment_id, database_name, family,
		 action_class, level, changed_by, change_reason, provenance)
		 VALUES ($1, 'orders', 'hygiene', 'analyze', 3, 'pg_sage', 'x', 'grandfathered')`,
		`INSERT INTO sage.sre_family_autonomy (deployment_id, database_name, family,
		 action_class, level, changed_by, change_reason, provenance)
		 VALUES ($1, 'orders', 'hygiene', 'reindex', 3, 'pg_sage', 'x', 'invented')`,
	}
	for _, stmt := range bad {
		if _, err := pool.Exec(ctx, stmt, trustDeployment); err == nil ||
			!strings.Contains(err.Error(), "check constraint") {
			t.Errorf("accepted %s: %v", strings.Fields(stmt)[2], err)
		}
	}
}

func TestTrustLedgerStateMarker(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	trustCleanup(t)
	insert := `INSERT INTO sage.trust_ledger_state (deployment_id, database_name,
		grandfathered_at, bound, seeded) VALUES ($1, 'orders', now(), '{}', '[]')
		ON CONFLICT (deployment_id, database_name) DO NOTHING`
	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, insert, trustDeployment); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.trust_ledger_state
		WHERE deployment_id = $1`, trustDeployment).Scan(&n); err != nil || n != 1 {
		t.Fatalf("markers = %d (%v)", n, err)
	}
}
