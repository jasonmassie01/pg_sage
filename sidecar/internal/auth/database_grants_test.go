package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// apiauth-08: per-database grants. A user with no grants keeps their
// global role everywhere. A user with any grant is scoped: on a granted
// database the lower of the global role and the grant wins, and they are
// a viewer everywhere else (including fleet-wide actions, database "").
// An admin is never scoped.

func TestEffectiveDatabaseRole(t *testing.T) {
	scoped := map[string]string{"orders": RoleOperator, "billing": RoleViewer}
	cases := []struct {
		name, global string
		grants       map[string]string
		db, want     string
	}{
		{"unscoped operator keeps global role", RoleOperator, nil, "orders", RoleOperator},
		{"unscoped viewer keeps global role", RoleViewer, map[string]string{}, "x", RoleViewer},
		{"unscoped operator on fleet-wide", RoleOperator, nil, "", RoleOperator},
		{"scoped operator on granted operator db", RoleOperator, scoped, "orders", RoleOperator},
		{"scoped operator on viewer grant", RoleOperator, scoped, "billing", RoleViewer},
		{"scoped operator on ungranted db", RoleOperator, scoped, "payroll", RoleViewer},
		{"scoped operator on fleet-wide", RoleOperator, scoped, "", RoleViewer},
		{"global viewer with operator grant stays viewer", RoleViewer, scoped, "orders",
			RoleViewer},
		{"admin is never scoped", RoleAdmin, scoped, "payroll", RoleAdmin},
		{"admin on fleet-wide", RoleAdmin, scoped, "", RoleAdmin},
		{"unknown global role gets nothing", "agent", nil, "orders", ""},
		{"unknown global role scoped gets nothing", "agent", scoped, "orders", ""},
		{"unknown grant role counts as no access", RoleOperator,
			map[string]string{"orders": "superuser"}, "orders", ""},
		{"empty database name in grants never matches fleet-wide", RoleOperator,
			map[string]string{"": RoleOperator}, "", RoleViewer},
	}
	for _, tc := range cases {
		if got := EffectiveDatabaseRole(tc.global, tc.grants, tc.db); got != tc.want {
			t.Errorf("%s: EffectiveDatabaseRole(%q, %v, %q) = %q, want %q", tc.name,
				tc.global, tc.grants, tc.db, got, tc.want)
		}
	}
}

func TestRoleAtLeast(t *testing.T) {
	cases := []struct {
		role, min string
		want      bool
	}{
		{RoleAdmin, RoleOperator, true},
		{RoleOperator, RoleOperator, true},
		{RoleViewer, RoleOperator, false},
		{RoleViewer, RoleViewer, true},
		{"", RoleViewer, false},
		{"agent", RoleViewer, false},
		{RoleAdmin, "", false}, // an unknown minimum fails closed
		{RoleAdmin, "root", false},
	}
	for _, tc := range cases {
		if got := RoleAtLeast(tc.role, tc.min); got != tc.want {
			t.Errorf("RoleAtLeast(%q, %q) = %v, want %v", tc.role, tc.min, got, tc.want)
		}
	}
}

func TestIsGrantRole(t *testing.T) {
	for role, want := range map[string]bool{RoleViewer: true, RoleOperator: true,
		RoleAdmin: false, "": false, "Operator": false} {
		if got := IsGrantRole(role); got != want {
			t.Errorf("IsGrantRole(%q) = %v, want %v", role, got, want)
		}
	}
}

func TestDatabaseGrantStore_NilPoolFailsClosed(t *testing.T) {
	var s *DatabaseGrantStore
	if _, err := s.UserDatabaseGrants(context.Background(), 1); !errors.Is(err,
		ErrGrantStoreUnavailable) {
		t.Fatalf("nil store: err = %v, want ErrGrantStoreUnavailable", err)
	}
	s = NewDatabaseGrantStore(nil)
	got, err := s.UserDatabaseGrants(context.Background(), 1)
	if !errors.Is(err, ErrGrantStoreUnavailable) || got != nil {
		t.Fatalf("nil pool: (%v, %v), want (nil, ErrGrantStoreUnavailable)", got, err)
	}
	if _, err := s.ListUserGrants(context.Background(), 1); !errors.Is(err,
		ErrGrantStoreUnavailable) {
		t.Fatalf("nil pool list: %v", err)
	}
}

func TestDatabaseGrantStore_InvalidInputRejectedBeforeSQL(t *testing.T) {
	// A nil pool proves validation runs before any query is attempted.
	s := NewDatabaseGrantStore(nil)
	ctx := context.Background()
	cases := []struct {
		name, db, role string
		want           error
	}{
		{"admin role", "orders", RoleAdmin, ErrInvalidGrantRole},
		{"empty role", "orders", "", ErrInvalidGrantRole},
		{"empty database", "", RoleOperator, ErrInvalidGrantDatabase},
		{"all databases", "all", RoleOperator, ErrInvalidGrantDatabase},
		{"wildcard", "*", RoleOperator, ErrInvalidGrantDatabase},
	}
	for _, tc := range cases {
		_, err := s.SetGrant(ctx, GrantChange{ActorID: 1, UserID: 2, Database: tc.db,
			Role: tc.role})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if err := s.RevokeGrant(ctx, GrantChange{ActorID: 1, UserID: 2, Database: ""}); !errors.Is(
		err, ErrInvalidGrantDatabase) {
		t.Errorf("revoke empty database: %v", err)
	}
}

func grantAuditCount(t *testing.T, s *DatabaseGrantStore, event string, target int) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.auth_audit
		WHERE event = $1 AND target_user_id = $2`, event, target).Scan(&n); err != nil {
		t.Fatalf("counting %s audit rows: %v", event, err)
	}
	return n
}

func TestDatabaseGrantStore_SetListRevokeAudited(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin := linkTestUser(t, pool, "grants-admin@example.com", RoleAdmin)
	op := linkTestUser(t, pool, "grants-op@example.com", RoleOperator)
	s := NewDatabaseGrantStore(pool)

	got, err := s.UserDatabaseGrants(ctx, op)
	if err != nil || len(got) != 0 {
		t.Fatalf("fresh user grants = (%v, %v), want empty", got, err)
	}
	g, err := s.SetGrant(ctx, GrantChange{ActorID: admin, UserID: op, Database: "orders",
		Role: RoleOperator, SourceIP: "10.0.0.9"})
	if err != nil {
		t.Fatalf("SetGrant: %v", err)
	}
	if g.UserID != op || g.Database != "orders" || g.Role != RoleOperator ||
		g.GrantedBy != admin || g.GrantedAt.IsZero() {
		t.Fatalf("SetGrant returned %+v", g)
	}
	// Re-granting changes the role in place (one row per user and database).
	if _, err := s.SetGrant(ctx, GrantChange{ActorID: admin, UserID: op,
		Database: "orders", Role: RoleViewer}); err != nil {
		t.Fatalf("SetGrant again: %v", err)
	}
	if _, err := s.SetGrant(ctx, GrantChange{ActorID: admin, UserID: op,
		Database: "billing", Role: RoleOperator}); err != nil {
		t.Fatalf("SetGrant billing: %v", err)
	}
	got, err = s.UserDatabaseGrants(ctx, op)
	if err != nil || len(got) != 2 || got["orders"] != RoleViewer ||
		got["billing"] != RoleOperator {
		t.Fatalf("grants = (%v, %v), want orders=viewer billing=operator", got, err)
	}
	list, err := s.ListUserGrants(ctx, op)
	if err != nil || len(list) != 2 || list[0].Database != "billing" ||
		list[1].Database != "orders" {
		t.Fatalf("ListUserGrants = (%+v, %v), want billing then orders", list, err)
	}
	if n := grantAuditCount(t, s, AuditDatabaseGrantSet, op); n != 3 {
		t.Fatalf("%d database_grant_set rows, want 3", n)
	}
	requireRevokeAudited(t, s, admin, op)
}

func requireRevokeAudited(t *testing.T, s *DatabaseGrantStore, admin, op int) {
	t.Helper()
	ctx := context.Background()
	if err := s.RevokeGrant(ctx, GrantChange{ActorID: admin, UserID: op,
		Database: "orders"}); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	if err := s.RevokeGrant(ctx, GrantChange{ActorID: admin, UserID: op,
		Database: "orders"}); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("second revoke: %v, want ErrGrantNotFound", err)
	}
	got, _ := s.UserDatabaseGrants(ctx, op)
	if len(got) != 1 || got["billing"] != RoleOperator {
		t.Fatalf("after revoke grants = %v, want billing only", got)
	}
	if n := grantAuditCount(t, s, AuditDatabaseGrantRevoked, op); n != 1 {
		t.Fatalf("%d database_grant_revoked rows, want 1 (a missed revoke is not audited)", n)
	}
}

func TestDatabaseGrantStore_UnknownUserAndCascade(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin := linkTestUser(t, pool, "grants-admin2@example.com", RoleAdmin)
	s := NewDatabaseGrantStore(pool)
	if _, err := s.SetGrant(ctx, GrantChange{ActorID: admin, UserID: 987654,
		Database: "orders", Role: RoleOperator}); !errors.Is(err, ErrGrantUserNotFound) {
		t.Fatalf("unknown user: %v, want ErrGrantUserNotFound", err)
	}
	if n := grantAuditCount(t, s, AuditDatabaseGrantSet, 987654); n != 0 {
		t.Fatalf("a refused grant wrote %d audit rows", n)
	}
	op := linkTestUser(t, pool, "grants-gone@example.com", RoleOperator)
	if _, err := s.SetGrant(ctx, GrantChange{ActorID: admin, UserID: op,
		Database: "orders", Role: RoleOperator}); err != nil {
		t.Fatal(err)
	}
	if err := deleteUserForTest(ctx, pool, op); err != nil {
		t.Fatalf("deleting a user with grants: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.user_database_grants
		WHERE user_id = $1`, op).Scan(&n); err != nil || n != 0 {
		t.Fatalf("grants of a deleted user: %d (%v), want 0", n, err)
	}
}

func TestDatabaseGrantStore_ConcurrentSetsKeepOneRow(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	admin := linkTestUser(t, pool, "grants-admin3@example.com", RoleAdmin)
	op := linkTestUser(t, pool, "grants-race@example.com", RoleOperator)
	s := NewDatabaseGrantStore(pool)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		role := RoleOperator
		if i%2 == 1 {
			role = RoleViewer
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.SetGrant(ctx, GrantChange{ActorID: admin, UserID: op,
				Database: "orders", Role: role})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent SetGrant: %v", err)
		}
	}
	list, err := s.ListUserGrants(ctx, op)
	if err != nil || len(list) != 1 || !IsGrantRole(list[0].Role) {
		t.Fatalf("after concurrent sets: (%+v, %v), want exactly one valid row", list, err)
	}
}
