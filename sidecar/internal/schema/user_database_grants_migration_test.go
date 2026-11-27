package schema

import (
	"context"
	"strings"
	"testing"
)

// apiauth-08: sage.user_database_grants holds one viewer or operator grant
// per user and database; it goes with its user, and bootstrap is
// idempotent over it.

func TestUserDatabaseGrantsMigration(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool) // a second run is a no-op
	if !strings.Contains(ddlUserDatabaseGrants, "lock_timeout") {
		t.Fatal("ddlUserDatabaseGrants must set lock_timeout")
	}
	uid := sessionsMigrationUser(t, ctx, pool, "grants-mig@example.com")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.users WHERE id = $1`, uid)
	})
	insert := `INSERT INTO sage.user_database_grants (user_id, database_name, role,
		granted_by) VALUES ($1, $2, $3, 0)`
	if _, err := pool.Exec(ctx, insert, uid, "orders", "operator"); err != nil {
		t.Fatalf("inserting an operator grant: %v", err)
	}
	for _, bad := range []struct{ name, db, role string }{
		{"admin role", "billing", "admin"},
		{"empty database", "", "viewer"},
		{"duplicate", "orders", "viewer"},
	} {
		if _, err := pool.Exec(ctx, insert, uid, bad.db, bad.role); err == nil {
			t.Errorf("%s accepted", bad.name)
		}
	}
	if _, err := pool.Exec(ctx, insert, 987654, "orders", "viewer"); err == nil {
		t.Error("a grant for a missing user was accepted")
	}
	var grantedAt bool
	if err := pool.QueryRow(ctx, `SELECT granted_at IS NOT NULL FROM sage.user_database_grants
		WHERE user_id = $1`, uid).Scan(&grantedAt); err != nil || !grantedAt {
		t.Fatalf("granted_at default: %v %v", grantedAt, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.users WHERE id = $1`, uid); err != nil {
		t.Fatalf("deleting a user with grants: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.user_database_grants
		WHERE user_id = $1`, uid).Scan(&n); err != nil || n != 0 {
		t.Fatalf("grants left after the user was deleted: %d %v", n, err)
	}
}
