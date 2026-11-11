package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// agentgov-06: sage.guard_principal_changes holds a widening principal
// change waiting for a second admin, at most one pending per principal.

func TestGuardPrincipalChangesMigration_TableAndChecks(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool) // idempotent: a second boot is a no-op
	const id = "agp_chgtestchgtestchgtes"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.guard_principal_changes
			WHERE principal_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.guard_principals WHERE id = $1`, id)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO sage.guard_principals (id, name, profile,
		env_ceiling, created_by) VALUES ($1, 'chg-mig-test', 'legacy', 'dev', 'mig-test')
		ON CONFLICT DO NOTHING`, id); err != nil {
		t.Fatalf("principal: %v", err)
	}
	insert := func(status string) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.guard_principal_changes (principal_id,
			patch, requested_by, requested_by_user, reason, expires_at, status)
			VALUES ($1, '{"env_ceiling":"prod"}', 'mig-test', 1, '', now(), $2)`, id, status)
		return err
	}
	if err := insert("pending"); err != nil {
		t.Fatalf("valid pending change: %v", err)
	}
	if err := insert("superseded"); err != nil {
		t.Fatalf("a closed change beside the pending one: %v", err)
	}
	err := insert("pending")
	if err == nil || !strings.Contains(err.Error(), "guard_principal_changes_pending") {
		t.Fatalf("a second pending change must hit the partial unique index: %v", err)
	}
	if err := insert("maybe"); err == nil {
		t.Fatal("an unknown status must be refused")
	}
	refuseBadChanges(t, ctx, pool, id)
}

// refuseBadChanges checks the table's constraints refuse malformed rows.
func refuseBadChanges(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) {
	t.Helper()
	for name, sql := range map[string]string{
		"unknown principal": `INSERT INTO sage.guard_principal_changes (principal_id, patch,
			requested_by, requested_by_user, expires_at)
			VALUES ('agp_nonenonenonenonenone', '{}', 'mig-test', 1, now())`,
		"array patch": `INSERT INTO sage.guard_principal_changes (principal_id, patch,
			requested_by, requested_by_user, expires_at, status)
			VALUES ('` + id + `', '[]', 'mig-test', 1, now(), 'expired')`,
		"no requester": `INSERT INTO sage.guard_principal_changes (principal_id, patch,
			requested_by, requested_by_user, expires_at, status)
			VALUES ('` + id + `', '{}', '', 1, now(), 'expired')`,
		"user zero": `INSERT INTO sage.guard_principal_changes (principal_id, patch,
			requested_by, requested_by_user, expires_at, status)
			VALUES ('` + id + `', '{}', 'mig-test', 0, now(), 'expired')`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}
