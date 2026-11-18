package schema

import (
	"context"
	"strings"
	"testing"
)

// On a real server: bootstrap twice is clean; existing rows read 'api';
// a grant may be 'config' but no other owner; managed_by takes only
// 'api' or 'config'.
func TestGitopsManagedByMigration_BootstrapTwiceAndChecked(t *testing.T) {
	pool := freshSchemaPool(t, "gitopsowner")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var owner string
	if err := pool.QueryRow(ctx, `SELECT managed_by FROM sage.policy_sets
		WHERE name = 'default'`).Scan(&owner); err != nil || owner != "api" {
		t.Fatalf("seeded default set managed_by = %q (%v), want api", owner, err)
	}
	var uid int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.users (email, password, role)
		VALUES ('g@example.com', 'x', 'viewer') RETURNING id`).Scan(&uid); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.user_database_grants
		(user_id, database_name, role, source) VALUES ($1, 'orders', 'viewer', 'config')`,
		uid); err != nil {
		t.Fatalf("config grant refused: %v", err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.user_database_grants
		(user_id, database_name, role, source) VALUES ($1, 'billing', 'viewer', 'git')`, uid)
	if err == nil || !strings.Contains(err.Error(), "user_database_grants_source_v2") {
		t.Fatalf("unknown grant source accepted or wrong error: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE sage.policy_sets SET managed_by = 'file'`)
	if err == nil || !strings.Contains(err.Error(), "policy_sets_managed_by") {
		t.Fatalf("unknown set owner accepted or wrong error: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.guard_principals (id, name, profile,
		created_by, managed_by) VALUES ('agp_aaaaaaaaaaaaaaaaaaaa', 'bot-x', 'reader', 't',
		'file')`)
	if err == nil || !strings.Contains(err.Error(), "guard_principals_managed_by") {
		t.Fatalf("unknown principal owner accepted or wrong error: %v", err)
	}
}
