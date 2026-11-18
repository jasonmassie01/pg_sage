package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cli-02a: a grant a GitOps file owns (source config) is written only by
// the reconciler's transaction calls; the admin editor refuses it, and the
// reconciler's revoke refuses a manual grant.
func TestConfigGrants_OwnedByTheFileOnly(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin := linkTestUser(t, pool, "cfg-admin@example.com", RoleAdmin)
	op := linkTestUser(t, pool, "cfg-op@example.com", RoleOperator)
	s := NewDatabaseGrantStore(pool)
	inTx := func(fn func(pgx.Tx) error) error { return pgx.BeginFunc(ctx, pool, fn) }
	file := "config:grants/a.yaml@abc"
	if err := inTx(func(tx pgx.Tx) error {
		return SetConfigGrantTx(ctx, tx, GrantChange{UserID: op, Database: "orders",
			Role: RoleViewer}, file)
	}); err != nil {
		t.Fatalf("SetConfigGrantTx: %v", err)
	}
	list, err := s.ListUserGrants(ctx, op)
	if err != nil || len(list) != 1 || list[0].Source != GrantSourceConfig ||
		list[0].GrantedBy != 0 {
		t.Fatalf("grants = %+v, %v; want one config grant", list, err)
	}
	_, err = s.SetGrant(ctx, GrantChange{ActorID: admin, UserID: op, Database: "orders",
		Role: RoleOperator})
	if !errors.Is(err, ErrConfigGrantReadOnly) {
		t.Fatalf("admin SetGrant on a config grant = %v, want ErrConfigGrantReadOnly", err)
	}
	err = s.RevokeGrant(ctx, GrantChange{ActorID: admin, UserID: op, Database: "orders"})
	if !errors.Is(err, ErrConfigGrantReadOnly) {
		t.Fatalf("admin RevokeGrant on a config grant = %v, want ErrConfigGrantReadOnly", err)
	}
	requireConfigRevoke(t, pool, admin, op, file)
	requireConfigRevokeAudit(t, pool, op, file)
	err = inTx(func(tx pgx.Tx) error {
		return SetConfigGrantTx(ctx, tx, GrantChange{UserID: op, Database: "orders",
			Role: RoleAdmin}, file)
	})
	if !errors.Is(err, ErrInvalidGrantRole) {
		t.Fatalf("config grant role admin = %v, want ErrInvalidGrantRole", err)
	}
}

func requireConfigRevokeAudit(t *testing.T, pool *pgxpool.Pool, op int, file string) {
	t.Helper()
	var actor string
	if err := pool.QueryRow(context.Background(), `SELECT detail->>'actor' FROM sage.auth_audit
		WHERE event = $1 AND target_user_id = $2`, AuditDatabaseGrantRevoked, op).
		Scan(&actor); err != nil || actor != file {
		t.Fatalf("revoke audit actor = %q, %v", actor, err)
	}
}

// requireConfigRevoke: the reconciler's revoke refuses a manual grant and
// removes its own.
func requireConfigRevoke(t *testing.T, pool *pgxpool.Pool, admin, op int, file string) {
	t.Helper()
	ctx := context.Background()
	s := NewDatabaseGrantStore(pool)
	inTx := func(fn func(pgx.Tx) error) error { return pgx.BeginFunc(ctx, pool, fn) }
	if _, err := s.SetGrant(ctx, GrantChange{ActorID: admin, UserID: op,
		Database: "billing", Role: RoleViewer}); err != nil {
		t.Fatal(err)
	}
	err := inTx(func(tx pgx.Tx) error {
		return RevokeConfigGrantTx(ctx, tx, GrantChange{UserID: op, Database: "billing"}, file)
	})
	if !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("config revoke of a manual grant = %v, want ErrGrantNotFound", err)
	}
	if err := inTx(func(tx pgx.Tx) error {
		return RevokeConfigGrantTx(ctx, tx, GrantChange{UserID: op, Database: "orders"}, file)
	}); err != nil {
		t.Fatalf("config revoke: %v", err)
	}
	got, err := s.UserDatabaseGrants(ctx, op)
	if err != nil || len(got) != 1 || got["billing"] != RoleViewer {
		t.Fatalf("grants after revoke = %v, %v; want only the manual billing grant", got, err)
	}
}
