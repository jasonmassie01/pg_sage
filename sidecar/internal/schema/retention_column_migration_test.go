package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const d5LegacyEvidence = "ev-d5-legacy-retention-contract"

// D5: an install that predates the owner-declared retention column gains it
// on upgrade, idempotently, and legacy contracts are NOT backfilled with an
// inferred column: they stay NULL so retention parks until re-declared.
func TestMigrationAddsRetentionColumnWithoutBackfill(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	simulatePreD5Schema(t, ctx, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.table_contract WHERE evidence_id=$1", d5LegacyEvidence)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.table_contract
		(schema_name, table_name, append_only, retention_interval, declared_by, evidence_id)
		VALUES ('public','d5_legacy_events',true,interval '30 days','legacy',$1)`,
		d5LegacyEvidence); err != nil {
		t.Fatalf("insert legacy contract: %v", err)
	}

	for run := 0; run < 2; run++ { // second run proves idempotence
		bootstrapWithRetry(t, ctx, pool)
	}

	for _, want := range []struct{ table, column, dataType string }{
		{"table_contract", "retention_column", "text"},
		{"retention_run", "relation_oid", "oid"},
		{"retention_run", "column_attnum", "smallint"},
		{"retention_run", "column_type", "text"},
		{"retention_run", "contract_id", "bigint"},
		{"retention_run", "contract_updated_at", "timestamp with time zone"},
	} {
		requireNullableColumn(t, ctx, pool, want.table, want.column, want.dataType)
	}
	var backfilled *string
	if err := pool.QueryRow(ctx, `SELECT retention_column FROM sage.table_contract
		WHERE evidence_id=$1`, d5LegacyEvidence).Scan(&backfilled); err != nil {
		t.Fatalf("read legacy contract: %v", err)
	}
	if backfilled != nil {
		t.Fatalf("legacy contract backfilled with retention_column=%q, want NULL", *backfilled)
	}
}

func simulatePreD5Schema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
ALTER TABLE sage.table_contract DROP COLUMN IF EXISTS retention_column;
ALTER TABLE sage.retention_run
    DROP COLUMN IF EXISTS relation_oid, DROP COLUMN IF EXISTS column_attnum,
    DROP COLUMN IF EXISTS column_type, DROP COLUMN IF EXISTS contract_id,
    DROP COLUMN IF EXISTS contract_updated_at`)
	if err != nil {
		t.Fatalf("simulate pre-D5 schema: %v", err)
	}
}

func requireNullableColumn(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, column, dataType string,
) {
	t.Helper()
	var gotType, nullable string
	err := pool.QueryRow(ctx, `SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_schema='sage' AND table_name=$1 AND column_name=$2`, table, column).
		Scan(&gotType, &nullable)
	if err != nil {
		t.Fatalf("sage.%s.%s missing after upgrade: %v", table, column, err)
	}
	if gotType != dataType || nullable != "YES" {
		t.Fatalf("sage.%s.%s = %s nullable=%s, want %s nullable", table, column,
			gotType, nullable, dataType)
	}
}
