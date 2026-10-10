package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// ErrOAuthUnmapped is an SSO user in no mapped group while
// oauth.unmapped_users is deny.
var ErrOAuthUnmapped = errors.New("oauth: user is in no group mapped to a role")

// rolePrivilege orders roles; a higher rank grants more.
var rolePrivilege = map[string]int{RoleViewer: 1, RoleOperator: 2, RoleAdmin: 3}

// RoleMappingConfigured reports whether IdP groups decide roles.
func RoleMappingConfigured(cfg *config.OAuthConfig) bool {
	return cfg != nil && len(cfg.RoleMapping) > 0
}

// ResolveOAuthRole returns the role an SSO login gets. Without a mapping it
// is the default role (viewer when unset). With one, the highest role of
// any matching group wins (exact, case-sensitive match); a user in no
// mapped group is denied or gets the default role per unmapped_users.
func ResolveOAuthRole(cfg *config.OAuthConfig, groups []string) (string, error) {
	fallback := RoleViewer
	if cfg != nil && cfg.DefaultRole != "" {
		fallback = cfg.DefaultRole
	}
	if !IsValidRole(fallback) {
		return "", fmt.Errorf("oauth: invalid default role %q", fallback)
	}
	if !RoleMappingConfigured(cfg) {
		return fallback, nil
	}
	member := make(map[string]bool, len(groups))
	for _, g := range groups {
		member[g] = true
	}
	best := ""
	for _, m := range cfg.RoleMapping {
		if member[m.Group] && rolePrivilege[m.Role] > rolePrivilege[best] {
			best = m.Role
		}
	}
	if best != "" {
		return best, nil
	}
	if cfg.UnmappedUsers == config.UnmappedUsersDefaultRole {
		return fallback, nil
	}
	return "", ErrOAuthUnmapped
}

// SyncOAuthUserRole sets an SSO user's role to the one their groups map
// to. It returns the previous role and whether it changed. The last-admin
// rule still holds: such a demotion fails with ErrLastAdmin.
func SyncOAuthUserRole(
	ctx context.Context, pool *pgxpool.Pool, userID int, role string,
) (string, bool, error) {
	if !IsValidRole(role) {
		return "", false, fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
	old, err := UserRole(ctx, pool, userID)
	if err != nil {
		return "", false, err
	}
	if old == role {
		return old, false, nil
	}
	if err := UpdateUserRolePreservingAdmin(ctx, pool, userID, role); err != nil {
		return old, false, err
	}
	return old, true, nil
}

// UserRole returns a user's current role.
func UserRole(ctx context.Context, pool *pgxpool.Pool, userID int) (string, error) {
	var role string
	err := pool.QueryRow(ctx,
		"/* pg_sage auth_user_role v1 */ SELECT role FROM sage.users WHERE id = $1",
		userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading role of user %d: %w", userID, err)
	}
	return role, nil
}

// UserIDByEmail returns the id of the user with this exact email, so a
// failed login can be audited against the account it targeted.
func UserIDByEmail(ctx context.Context, pool *pgxpool.Pool, email string) (int, error) {
	var id int
	err := pool.QueryRow(ctx,
		"/* pg_sage auth_user_lookup v1 */ SELECT id FROM sage.users WHERE email = $1",
		email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrUserNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("looking up user by email: %w", err)
	}
	return id, nil
}
