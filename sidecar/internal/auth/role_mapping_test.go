package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func mappingConfig(unmapped string) *config.OAuthConfig {
	return &config.OAuthConfig{
		DefaultRole: RoleViewer, UnmappedUsers: unmapped, GroupsClaim: "groups",
		RoleMapping: []config.OAuthRoleMapping{
			{Group: "pg-viewers", Role: RoleViewer},
			{Group: "pg-admins", Role: RoleAdmin},
			{Group: "pg-operators", Role: RoleOperator},
		},
	}
}

func TestResolveOAuthRole(t *testing.T) {
	cases := map[string]struct {
		cfg     *config.OAuthConfig
		groups  []string
		want    string
		wantErr error
	}{
		"no mapping uses default role": {
			&config.OAuthConfig{DefaultRole: RoleOperator}, []string{"x"}, RoleOperator, nil},
		"no mapping empty default is viewer": {
			&config.OAuthConfig{}, nil, RoleViewer, nil},
		"single match": {
			mappingConfig("deny"), []string{"pg-operators"}, RoleOperator, nil},
		"highest privilege wins": {
			mappingConfig("deny"), []string{"pg-viewers", "pg-admins", "pg-operators"},
			RoleAdmin, nil},
		"order independent": {
			mappingConfig("deny"), []string{"pg-operators", "pg-viewers"}, RoleOperator, nil},
		"unmapped denied": {
			mappingConfig("deny"), []string{"engineering"}, "", ErrOAuthUnmapped},
		"no groups denied": {
			mappingConfig("deny"), nil, "", ErrOAuthUnmapped},
		"empty groups denied": {
			mappingConfig("deny"), []string{}, "", ErrOAuthUnmapped},
		"unmapped gets default role": {
			mappingConfig("default_role"), []string{"engineering"}, RoleViewer, nil},
		"case sensitive match": {
			mappingConfig("deny"), []string{"PG-ADMINS"}, "", ErrOAuthUnmapped},
		"whitespace is not trimmed": {
			mappingConfig("deny"), []string{" pg-admins"}, "", ErrOAuthUnmapped},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ResolveOAuthRole(tc.cfg, tc.groups)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("role = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveOAuthRole_NilConfig(t *testing.T) {
	got, err := ResolveOAuthRole(nil, []string{"pg-admins"})
	if err != nil || got != RoleViewer {
		t.Fatalf("nil config = (%q, %v), want viewer", got, err)
	}
}

func TestResolveOAuthRole_InvalidDefaultRoleIsAnError(t *testing.T) {
	_, err := ResolveOAuthRole(&config.OAuthConfig{DefaultRole: "root"}, nil)
	if err == nil {
		t.Fatal("invalid default role accepted")
	}
}

func TestRoleMappingConfigured(t *testing.T) {
	if RoleMappingConfigured(nil) || RoleMappingConfigured(&config.OAuthConfig{}) {
		t.Fatal("empty mapping reported as configured")
	}
	if !RoleMappingConfigured(mappingConfig("deny")) {
		t.Fatal("mapping not reported as configured")
	}
}

func TestSyncOAuthUserRole(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	keeper := linkTestUser(t, pool, "keeper-admin@example.com", RoleAdmin)
	_ = keeper
	id := linkTestUser(t, pool, "synced@example.com", RoleAdmin)

	old, changed, err := SyncOAuthUserRole(ctx, pool, id, RoleViewer)
	if err != nil || !changed || old != RoleAdmin {
		t.Fatalf("demote = (%q, %v, %v), want (admin, true, nil)", old, changed, err)
	}
	if role, _ := UserRole(ctx, pool, id); role != RoleViewer {
		t.Fatalf("stored role = %q, want viewer", role)
	}
	old, changed, err = SyncOAuthUserRole(ctx, pool, id, RoleViewer)
	if err != nil || changed || old != RoleViewer {
		t.Fatalf("same role = (%q, %v, %v), want (viewer, false, nil)", old, changed, err)
	}
}

func TestSyncOAuthUserRole_RefusesLastAdminDemotion(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "UPDATE sage.users SET role = 'viewer'"); err != nil {
		t.Fatal(err)
	}
	id := linkTestUser(t, pool, "only-admin@example.com", RoleAdmin)
	_, changed, err := SyncOAuthUserRole(ctx, pool, id, RoleOperator)
	if !errors.Is(err, ErrLastAdmin) || changed {
		t.Fatalf("last admin demotion = (%v, %v), want ErrLastAdmin", changed, err)
	}
	if role, _ := UserRole(ctx, pool, id); role != RoleAdmin {
		t.Fatalf("role after refused demotion = %q, want admin", role)
	}
}

func TestSyncOAuthUserRole_InvalidInput(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := linkTestUser(t, pool, "invalid-sync@example.com", RoleViewer)
	if _, _, err := SyncOAuthUserRole(ctx, pool, id, "root"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("invalid role: err = %v, want ErrInvalidRole", err)
	}
	if _, _, err := SyncOAuthUserRole(ctx, pool, 987654, RoleViewer); !errors.Is(
		err, ErrUserNotFound) {
		t.Fatalf("missing user: err = %v, want ErrUserNotFound", err)
	}
}

func TestUserRoleAndUserIDByEmail(t *testing.T) {
	pool := setupOIDCPool(t)
	ctx := context.Background()
	id := linkTestUser(t, pool, "lookup@example.com", RoleOperator)
	if role, err := UserRole(ctx, pool, id); err != nil || role != RoleOperator {
		t.Fatalf("UserRole = (%q, %v)", role, err)
	}
	if got, err := UserIDByEmail(ctx, pool, "lookup@example.com"); err != nil || got != id {
		t.Fatalf("UserIDByEmail = (%d, %v), want %d", got, err, id)
	}
	if _, err := UserIDByEmail(ctx, pool, "absent@example.com"); !errors.Is(
		err, ErrUserNotFound) {
		t.Fatalf("absent email: err = %v, want ErrUserNotFound", err)
	}
	if _, err := UserRole(ctx, pool, 999999); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("absent id: err = %v, want ErrUserNotFound", err)
	}
}
