//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// apiauth-01: every config change snapshots the caller's email into the
// sealed changed_by_actor column, so the audit trail still names the person
// after their account is deleted. changed_by keeps the id without a foreign
// key.

func latestAuditActor(
	t *testing.T, pool *pgxpool.Pool, key string,
) (changedBy *int, actor *string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx, `SELECT changed_by, changed_by_actor
		FROM sage.config_audit WHERE key = $1 ORDER BY id DESC LIMIT 1`, key).
		Scan(&changedBy, &actor); err != nil {
		t.Fatalf("reading audit row for %s: %v", key, err)
	}
	return changedBy, actor
}

// actorProbeKey is a real, non-secret key: SetOverride validates keys.
const actorProbeKey = "llm.timeout_seconds"

func clearTimeoutOverride(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	clear := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.config
			WHERE key = $1 AND database_id IS NULL`, actorProbeKey)
	}
	clear()
	t.Cleanup(clear)
}

func TestConfigAudit_SnapshotsActorEmail(t *testing.T) {
	pool := setupConfigTestDB(t)
	cs := NewConfigStore(pool)
	ctx := context.Background()
	clearTimeoutOverride(t, pool)
	if err := cs.SetOverride(ctx, actorProbeKey, "45", 0, 1); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	by, actor := latestAuditActor(t, pool, actorProbeKey)
	if by == nil || *by != 1 {
		t.Fatalf("changed_by = %v, want 1", by)
	}
	if actor == nil || *actor != "test@test.com" {
		t.Fatalf("changed_by_actor = %v, want test@test.com", actor)
	}
}

func TestConfigAudit_NoUserLeavesActorEmpty(t *testing.T) {
	pool := setupConfigTestDB(t)
	cs := NewConfigStore(pool)
	clearTimeoutOverride(t, pool)
	if err := cs.SetOverride(context.Background(), actorProbeKey, "46", 0, 0); err != nil {
		t.Fatalf("SetOverride: %v", err)
	}
	by, actor := latestAuditActor(t, pool, actorProbeKey)
	if by != nil || actor != nil {
		t.Fatalf("system change: changed_by=%v actor=%v, want both NULL", by, actor)
	}
}

// A user id whose row lives elsewhere (fleet mode keeps users on the auth
// database) is kept as an id with no actor, and is no longer refused.
func TestConfigAudit_UnknownUserKeepsIDWithoutActor(t *testing.T) {
	pool := setupConfigTestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.users WHERE id = 987650`); err != nil {
		t.Fatalf("clearing probe user: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := insertAudit(ctx, tx, "test.actor_unknown", "", "1", 0, 987650); err != nil {
		t.Fatalf("insertAudit refused an id without a users row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	by, actor := latestAuditActor(t, pool, "test.actor_unknown")
	if by == nil || *by != 987650 || actor != nil {
		t.Fatalf("changed_by=%v actor=%v, want 987650 and NULL", by, actor)
	}
}
