package schema

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// apiauth-03: sage.users gains must_change_password (boolean NOT NULL
// DEFAULT false) and password_changed_at (timestamptz), additively and
// idempotently. Existing accounts are not forced to change on upgrade.

func usersColumnType(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	column string) (string, bool, string) {
	t.Helper()
	var typ, def string
	var notNull bool
	err := pool.QueryRow(ctx, `SELECT format_type(a.atttypid, a.atttypmod), a.attnotnull,
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), '')
		  FROM pg_attribute a
		  LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		 WHERE a.attrelid = 'sage.users'::regclass
		   AND a.attname = $1 AND NOT a.attisdropped`, column).Scan(&typ, &notNull, &def)
	if err != nil {
		t.Fatalf("reading sage.users.%s: %v", column, err)
	}
	return typ, notNull, def
}

func requirePasswordLifecycleColumns(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	typ, notNull, def := usersColumnType(t, ctx, pool, "must_change_password")
	if typ != "boolean" || !notNull || def != "false" {
		t.Fatalf("must_change_password = %s notnull=%v default=%q, want boolean NOT NULL "+
			"DEFAULT false", typ, notNull, def)
	}
	typ, notNull, _ = usersColumnType(t, ctx, pool, "password_changed_at")
	if typ != "timestamp with time zone" || notNull {
		t.Fatalf("password_changed_at = %s notnull=%v, want nullable timestamptz", typ, notNull)
	}
}

func TestUsersPasswordLifecycleMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	requirePasswordLifecycleColumns(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	requirePasswordLifecycleColumns(t, ctx, pool)

	// An install from before apiauth-03 with an existing user.
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.users
		DROP COLUMN IF EXISTS must_change_password,
		DROP COLUMN IF EXISTS password_changed_at`); err != nil {
		t.Fatalf("simulating the old schema: %v", err)
	}
	uid := sessionsMigrationUser(t, ctx, pool, "pw-lifecycle-mig@example.com")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.users WHERE id = $1`, uid)
	})
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	requirePasswordLifecycleColumns(t, ctx, pool)
	var forced, changed bool
	if err := pool.QueryRow(ctx, `SELECT must_change_password,
		password_changed_at IS NOT NULL FROM sage.users WHERE id = $1`, uid).
		Scan(&forced, &changed); err != nil {
		t.Fatalf("reading migrated user: %v", err)
	}
	if forced || changed {
		t.Fatalf("existing user forced=%v changed_at set=%v by the upgrade, want neither",
			forced, changed)
	}
}

func TestUsersPasswordLifecycleMigrationRunsAfterDisabledAt(t *testing.T) {
	disabled, lifecycle := -1, -1
	for i, s := range migrationSteps() {
		switch s.name {
		case "ddlUsersDisabledAt":
			disabled = i
		case "ddlUsersPasswordLifecycle":
			lifecycle = i
			if s.sql != ddlUsersPasswordLifecycle {
				t.Fatal("ddlUsersPasswordLifecycle step carries other SQL")
			}
		}
	}
	if lifecycle < 0 || lifecycle < disabled {
		t.Fatalf("ddlUsersPasswordLifecycle at %d, ddlUsersDisabledAt at %d", lifecycle,
			disabled)
	}
}
