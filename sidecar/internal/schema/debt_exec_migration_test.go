package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Debt items 1-3: typed-target lease identity on sage.change_lease, the
// durable serialize_mode=queue table and the retention action/dry-run
// links arrive on upgrade, idempotently, without touching existing rows.
func TestDebtExecMigrationAddsLeaseQueueAndRetentionLinks(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	simulatePreDebtExecSchema(t, ctx, pool)

	for run := 0; run < 2; run++ { // second run proves idempotence
		bootstrapWithRetry(t, ctx, pool)
	}

	for _, want := range []struct{ table, column, dataType string }{
		{"change_lease", "actor", "text"},
		{"change_lease", "object_type", "text"},
		{"change_lease", "object_oid", "oid"},
		{"change_lease", "object_name", "text"},
		{"retention_run", "action_id", "bigint"},
		{"retention_run", "dry_run_id", "bigint"},
		{"lease_queue", "decision_id", "bigint"},
		{"lease_queue", "resolved_at", "timestamp with time zone"},
	} {
		requireNullableColumn(t, ctx, pool, want.table, want.column, want.dataType)
	}
	for _, column := range []string{"request_key", "object_keys", "kind", "actor",
		"intent", "instance", "state", "enqueued_at", "heartbeat_at", "deadline_at",
		"resumed_count"} {
		requireNotNullColumn(t, ctx, pool, "lease_queue", column)
	}
}

func TestLeaseQueueRejectsUnknownStateAndDuplicateWaiters(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	insert := `INSERT INTO sage.lease_queue (request_key, object_keys, kind, actor,
		intent, instance, state, deadline_at)
		VALUES ($1, ARRAY['public.t'], 'finding', 'executor', 'x', 'i', $2,
		        now() + interval '1 minute')`
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.lease_queue WHERE request_key LIKE 'migration-test-%'")
	})
	if _, err := pool.Exec(ctx, insert, "migration-test-bogus", "bogus"); err == nil {
		t.Fatal("lease_queue accepted state 'bogus'")
	}
	if _, err := pool.Exec(ctx, insert, "migration-test-dup", "waiting"); err != nil {
		t.Fatalf("first waiting entry: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "migration-test-dup", "waiting"); err == nil {
		t.Fatal("two waiting entries for one request were accepted")
	}
	if _, err := pool.Exec(ctx, insert, "migration-test-dup", "timed_out"); err != nil {
		t.Fatalf("a resolved entry beside a waiting one was refused: %v", err)
	}
}

func simulatePreDebtExecSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
DROP TABLE IF EXISTS sage.lease_queue;
ALTER TABLE sage.change_lease
    DROP COLUMN IF EXISTS actor, DROP COLUMN IF EXISTS object_type,
    DROP COLUMN IF EXISTS object_oid, DROP COLUMN IF EXISTS object_name;
ALTER TABLE sage.retention_run
    DROP COLUMN IF EXISTS action_id, DROP COLUMN IF EXISTS dry_run_id`)
	if err != nil {
		t.Fatalf("simulate pre-debt-exec schema: %v", err)
	}
}

func requireNotNullColumn(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, column string,
) {
	t.Helper()
	var nullable string
	err := pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns
		WHERE table_schema='sage' AND table_name=$1 AND column_name=$2`, table, column).
		Scan(&nullable)
	if err != nil {
		t.Fatalf("sage.%s.%s missing: %v", table, column, err)
	}
	if nullable != "NO" {
		t.Fatalf("sage.%s.%s is nullable, want NOT NULL", table, column)
	}
}
