package mcptoken

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Validate authenticates secret. Anything wrong with it (malformed,
// unknown, revoked, expired, owner gone) is ErrUnauthorized; a storage
// failure is a different, wrapped error. An operator token's scopes are
// narrowed to the owner's current role. last_used_at is recorded at most
// once a minute per token.
func (s *Store) Validate(ctx context.Context, secret string) (Grant, error) {
	if !wellFormedSecret(secret) {
		return Grant{}, ErrUnauthorized
	}
	if err := s.ready(); err != nil {
		return Grant{}, err
	}
	var g Grant
	var kind string
	var owner *int
	var role *string
	err := s.pool.QueryRow(ctx, `/* pg_sage */
		WITH hit AS (
			SELECT t.id, t.name, t.kind, t.scopes, t.databases, t.owner_user_id,
				t.last_used_at, u.role
			FROM sage.mcp_tokens t
			LEFT JOIN sage.users u ON u.id = t.owner_user_id
			WHERE t.token_hash = $1 AND t.revoked_at IS NULL AND t.expires_at > now()
		), touched AS (
			UPDATE sage.mcp_tokens t SET last_used_at = now()
			FROM hit
			WHERE t.id = hit.id AND (hit.last_used_at IS NULL
				OR hit.last_used_at < now() - interval '1 minute')
		)
		SELECT id::text, name, kind, scopes, databases, owner_user_id, role FROM hit`,
		HashSecret(secret)).Scan(&g.TokenID, &g.Name, &kind, &g.Scopes, &g.Databases,
		&owner, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, ErrUnauthorized
	}
	if err != nil {
		return Grant{}, fmt.Errorf("mcptoken: validating token: %w", err)
	}
	g.Kind = Kind(kind)
	if len(g.Databases) == 1 && g.Databases[0] == allDatabases {
		g.Databases = nil
	}
	if g.Kind == KindAgent {
		return g, nil
	}
	return narrowToOwner(g, owner, role)
}

// narrowToOwner limits an operator token to its owner's current role.
func narrowToOwner(g Grant, owner *int, role *string) (Grant, error) {
	if owner == nil || role == nil {
		return Grant{}, ErrUnauthorized
	}
	allowed := roleScopes(*role)
	scopes := make([]string, 0, len(g.Scopes))
	for _, s := range g.Scopes {
		if allowed[s] {
			scopes = append(scopes, s)
		}
	}
	if len(scopes) == 0 {
		return Grant{}, ErrUnauthorized
	}
	g.Scopes, g.OwnerUserID, g.OwnerRole = scopes, *owner, *role
	return g, nil
}
