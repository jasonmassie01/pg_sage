package mcptoken_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// apiauth-01: a deactivated user's power must not survive through an
// operator MCP token. SetUserDisabled revokes the tokens a user owns at
// that moment; these tests set disabled_at directly, so they prove that
// the token store itself refuses a deactivated owner (a token minted by
// another admin afterwards, or one that raced the deactivation).

func setDisabled(t *testing.T, pool *pgxpool.Pool, id int, disabled bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE sage.users
		SET disabled_at = CASE WHEN $2 THEN now() END WHERE id = $1`, id, disabled)
	require.NoError(t, err)
}

func TestValidateRefusesDeactivatedOwner(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	owner := createUser(t, pool, auth.RoleAdmin)
	tok := mustCreate(t, s, operatorReq(uniq("op-disabled"), owner))
	requireGrant(t, s, tok.Secret, auth.RoleAdmin, []string{"read", "propose", "approve"})

	setDisabled(t, pool, owner, true)
	grant, err := s.Validate(context.Background(), tok.Secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
	require.Equal(t, 0, grant.OwnerUserID)
	require.Equal(t, "", grant.TokenID)

	// The token row was not revoked here, so re-enabling the owner (the
	// state transition back) lets it authenticate again.
	setDisabled(t, pool, owner, false)
	requireGrant(t, s, tok.Secret, auth.RoleAdmin, []string{"read", "propose", "approve"})
}

func TestCreateRefusesDeactivatedOwner(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	owner := createUser(t, pool, auth.RoleOperator)
	setDisabled(t, pool, owner, true)
	name := uniq("op-disabled-owner")
	tok, err := s.Create(context.Background(), operatorReq(name, owner))
	require.ErrorIs(t, err, mcptoken.ErrOwnerRequired)
	require.Equal(t, "", tok.Secret)
	require.Equal(t, 0, rowCount(t, pool, name))

	setDisabled(t, pool, owner, false)
	ok := mustCreate(t, s, operatorReq(uniq("op-reenabled-owner"), owner))
	require.Equal(t, mcptoken.KindOperator, ok.Kind)
}
