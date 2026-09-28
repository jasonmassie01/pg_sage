package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const dedupeTable = "d5_dedupe_events"

// Installs that predate the identity index may hold several contracts for
// one table (NULL database_id is not deduplicated by the UNIQUE constraint).
// The upgrade keeps only the newest row per (database_id, schema, table),
// treating NULL database_id as one identity, then enforces it with an index.
func TestTableContractIdentityMigrationKeepsNewest(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	cleanupDedupeRows(t, ctx, pool)
	t.Cleanup(func() { cleanupDedupeRows(t, context.Background(), pool) })
	if _, err := pool.Exec(ctx,
		"DROP INDEX IF EXISTS sage.idx_table_contract_identity"); err != nil {
		t.Fatalf("simulate pre-index install: %v", err)
	}
	insertDedupeContract(t, ctx, pool, nil, "ev-dedupe-old", "3 days")
	insertDedupeContract(t, ctx, pool, nil, "ev-dedupe-newest", "1 day")
	insertDedupeContract(t, ctx, pool, nil, "ev-dedupe-middle", "2 days")
	databaseID := int64(990201)
	insertDedupeContract(t, ctx, pool, &databaseID, "ev-dedupe-other-db", "4 days")

	for run := 0; run < 2; run++ { // second run proves idempotence
		bootstrapWithRetry(t, ctx, pool)
	}

	rows, err := pool.Query(ctx, `SELECT evidence_id FROM sage.table_contract
		WHERE table_name=$1 ORDER BY evidence_id`, dedupeTable)
	if err != nil {
		t.Fatalf("read contracts: %v", err)
	}
	var kept []string
	for rows.Next() {
		var evidence string
		if err := rows.Scan(&evidence); err != nil {
			t.Fatalf("scan contract: %v", err)
		}
		kept = append(kept, evidence)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate contracts: %v", err)
	}
	if len(kept) != 2 || kept[0] != "ev-dedupe-newest" || kept[1] != "ev-dedupe-other-db" {
		t.Fatalf("kept contracts = %v, want [ev-dedupe-newest ev-dedupe-other-db]", kept)
	}
	requireDuplicateRejected(t, ctx, pool)
}

// insertDedupeContract backdates updated_at by age. The newest declaration
// ("1 day") is inserted second, so it does not have the highest id: the
// migration must keep the newest by updated_at, not the last inserted.
func insertDedupeContract(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	databaseID *int64, evidence, age string,
) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO sage.table_contract
		(database_id, schema_name, table_name, append_only, declared_by, evidence_id,
		 updated_at)
		VALUES ($1,'public',$2,true,'legacy',$3, now() - $4::interval)`,
		databaseID, dedupeTable, evidence, age)
	if err != nil {
		t.Fatalf("insert contract %s: %v", evidence, err)
	}
}

func requireDuplicateRejected(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO sage.table_contract
		(schema_name, table_name, declared_by, evidence_id)
		VALUES ('public',$1,'legacy','ev-dedupe-after-index')`, dedupeTable)
	if err == nil {
		t.Fatal("a second NULL database_id contract for the table was accepted")
	}
}

func cleanupDedupeRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, "DELETE FROM sage.table_contract WHERE table_name=$1",
		dedupeTable); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
