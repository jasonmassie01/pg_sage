package schema

import (
	"testing"
)

// earned-07: sage.sre_family_autonomy records the PostgreSQL major a level
// was granted on (server_major, nullable smallint) after bootstrap, run
// twice; the migration re-runs cleanly and refuses a nonsense major.
func TestSREMigrationServerMajor_AddsTheColumnIdempotently(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	var typ, nullable string
	if err := pool.QueryRow(ctx, `SELECT data_type, is_nullable
		FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'sre_family_autonomy'
		  AND column_name = 'server_major'`).Scan(&typ, &nullable); err != nil {
		t.Fatalf("server_major column: %v", err)
	}
	if typ != "smallint" || nullable != "YES" {
		t.Fatalf("server_major is %s nullable=%s, want a nullable smallint", typ, nullable)
	}
	// Rolled back: other packages share the database.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, ddlSREAutonomyServerMajor); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_family_autonomy
		(deployment_id, database_name, family, action_class, level, changed_by,
		 change_reason, server_major)
		VALUES (gen_random_uuid(), 'm', 'hygiene', 'vacuum', 1, 't', 't', NULL),
		       (gen_random_uuid(), 'm', 'hygiene', 'vacuum', 1, 't', 't', 17)`); err != nil {
		t.Fatalf("NULL and 17 refused: %v", err)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT bad`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.sre_family_autonomy
		(deployment_id, database_name, family, action_class, level, changed_by,
		 change_reason, server_major)
		VALUES (gen_random_uuid(), 'm', 'hygiene', 'vacuum', 1, 't', 't', 0)`); err == nil {
		t.Fatal("server_major 0 was accepted")
	}
}
