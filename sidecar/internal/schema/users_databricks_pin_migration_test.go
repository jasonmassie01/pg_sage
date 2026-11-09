package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// databricks-07: sage.users gains databricks_user_pin text, additively and
// idempotently. An install from before the column gets it on the next
// bootstrap with existing rows left NULL (unpinned).

func usersPinColumnType(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var typ string
	err := pool.QueryRow(ctx, `SELECT format_type(atttypid, atttypmod)
		  FROM pg_attribute
		 WHERE attrelid = 'sage.users'::regclass
		   AND attname = 'databricks_user_pin' AND NOT attisdropped`).Scan(&typ)
	if err != nil {
		t.Fatalf("reading databricks_user_pin column: %v", err)
	}
	return typ
}

func TestUsersDatabricksPinMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if typ := usersPinColumnType(t, ctx, pool); typ != "text" {
		t.Fatalf("databricks_user_pin type = %q, want text", typ)
	}
	// A second run on a migrated schema is a no-op.
	bootstrapWithRetry(t, ctx, pool)
	if typ := usersPinColumnType(t, ctx, pool); typ != "text" {
		t.Fatalf("after a second bootstrap: type = %q, want text", typ)
	}

	// Simulate a pre-databricks-07 install with an existing user.
	if _, err := pool.Exec(ctx,
		`ALTER TABLE sage.users DROP COLUMN IF EXISTS databricks_user_pin`); err != nil {
		t.Fatalf("simulating the old schema: %v", err)
	}
	uid := sessionsMigrationUser(t, ctx, pool, "pin-mig@example.com")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.users WHERE id = $1`, uid)
	})
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	var pin *string
	if err := pool.QueryRow(ctx, `SELECT databricks_user_pin FROM sage.users
		WHERE id = $1`, uid).Scan(&pin); err != nil {
		t.Fatalf("reading migrated user: %v", err)
	}
	if pin != nil {
		t.Fatalf("existing user pinned to %q by the migration, want NULL", *pin)
	}
}

func TestUsersDatabricksPinMigrationIsRegisteredLast(t *testing.T) {
	stmts := migrationStatements()
	idx := -1
	for i, s := range stmts {
		if s == ddlUsersDatabricksPin {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("ddlUsersDatabricksPin is not in the migration chain")
	}
	for i, s := range stmts {
		if s == ddlUsersDisabledAt && i > idx {
			t.Fatalf("pin migration at %d runs before disabled_at at %d", idx, i)
		}
	}
}
