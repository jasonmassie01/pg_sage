package agentguard

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

const day = 24 * time.Hour

func tokenReq(name string) TokenRequest {
	return TokenRequest{Name: name, Scopes: []string{"read", "propose"},
		Databases: []string{"orders"}, ExpiresIn: day, CreatedBy: "admin@example.com"}
}

func TestIssueToken_AuthenticatesAsThePrincipal(t *testing.T) {
	pool := livePool(t)
	s, tokens := NewStore(pool), mcptoken.NewStore(pool)
	ctx := context.Background()
	sponsor := createUser(t, pool, "admin")
	p := newPrincipal(t, s, &sponsor)
	tok, err := IssueToken(ctx, s, tokens, p.ID, tokenReq("ci-bot"))
	require.NoError(t, err)
	require.Equal(t, p.ID, tok.PrincipalID)
	require.Equal(t, mcptoken.KindAgent, tok.Kind)
	require.NotEmpty(t, tok.Secret)
	id, grant, err := Authenticate(ctx, s, tokens, tok.Secret)
	require.NoError(t, err)
	require.Equal(t, p.ID, grant.PrincipalID)
	require.Equal(t, p.ID, id.Principal.ID)
	require.Equal(t, p.Name, id.Principal.Name)
	require.True(t, id.Principal.Sponsored())
	require.Equal(t, tok.ID, id.TokenID)
	require.Equal(t, []string{"orders"}, id.Databases)
	require.True(t, id.MayUseDatabase("orders"))
	require.False(t, id.MayUseDatabase("billing"), "G1-03: a token for A is refused on B")
	listed, err := tokens.List(ctx)
	require.NoError(t, err)
	found := false
	for _, l := range listed {
		if l.ID == tok.ID {
			found = true
			require.Equal(t, p.ID, l.PrincipalID)
			require.Equal(t, "", l.Secret)
		}
	}
	require.True(t, found)
}

func TestIssueToken_Refusals(t *testing.T) {
	pool := livePool(t)
	s, tokens := NewStore(pool), mcptoken.NewStore(pool)
	ctx := context.Background()
	p := newPrincipal(t, s, nil)
	approve := tokenReq("x")
	approve.Scopes = []string{"read", "approve"}
	_, err := IssueToken(ctx, s, tokens, p.ID, approve)
	require.ErrorIs(t, err, ErrInvalid, "agents never hold approve")
	long := tokenReq("x")
	long.ExpiresIn = MaxTokenLifetime + time.Hour
	_, err = IssueToken(ctx, s, tokens, p.ID, long)
	require.ErrorIs(t, err, ErrInvalid)
	_, err = IssueToken(ctx, s, tokens, "agp_aaaaaaaaaaaaaaaaaaaa", tokenReq("x"))
	require.ErrorIs(t, err, ErrNotFound)
	_, err = IssueToken(ctx, s, nil, p.ID, tokenReq("x"))
	require.ErrorIs(t, err, ErrUnavailable)
	_, err = s.SetStatus(ctx, p.ID, StatusFrozen, "kill")
	require.NoError(t, err)
	_, err = IssueToken(ctx, s, tokens, p.ID, tokenReq("x"))
	d, ok := IsDenied(err)
	require.True(t, ok, "frozen: %v", err)
	require.Equal(t, ReasonFrozen, d.Reason)
	_, err = s.SetStatus(ctx, p.ID, StatusRetired, "")
	require.NoError(t, err)
	_, err = IssueToken(ctx, s, tokens, p.ID, tokenReq("x"))
	require.ErrorIs(t, err, ErrRetired)
}

func TestAuthenticate_FollowsPrincipalStatus(t *testing.T) {
	pool := livePool(t)
	s, tokens := NewStore(pool), mcptoken.NewStore(pool)
	ctx := context.Background()
	p := newPrincipal(t, s, nil)
	tok, err := IssueToken(ctx, s, tokens, p.ID, tokenReq("status-bot"))
	require.NoError(t, err)
	_, err = s.SetStatus(ctx, p.ID, StatusFrozen, "anomaly")
	require.NoError(t, err)
	id, _, err := Authenticate(ctx, s, tokens, tok.Secret)
	require.NoError(t, err, "a frozen principal still authenticates; D1 denies its actions")
	require.True(t, id.Principal.Frozen())
	require.Equal(t, ReasonFrozen, ToolAccess(id.Principal, ToolPropose).Reason)
	_, err = s.SetStatus(ctx, p.ID, StatusRetired, "")
	require.NoError(t, err)
	_, _, err = Authenticate(ctx, s, tokens, tok.Secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
	_, _, err = Authenticate(ctx, s, tokens, "pgs_mcp_not-a-token")
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
}

func TestAgentTokenWithoutPrincipal_FailsClosed(t *testing.T) {
	pool := livePool(t)
	tokens := mcptoken.NewStore(pool)
	ctx := context.Background()
	_, err := tokens.Create(ctx, mcptoken.CreateRequest{Name: "loose", Kind: mcptoken.KindAgent,
		Scopes: []string{"read"}, Databases: []string{"*"}, ExpiresIn: day, CreatedBy: "a"})
	require.ErrorIs(t, err, mcptoken.ErrPrincipalRequired)
	// A legacy row (pre-G1) that the migration has not bound yet.
	secret := "pgs_mcp_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	_, err = pool.Exec(ctx, `INSERT INTO sage.mcp_tokens (name, kind, scopes, databases,
		token_hash, prefix, created_by, expires_at)
		VALUES ('legacy', 'agent', '{read}', '{*}', $1, 'pgs_mcp_AAAA', 'a',
		now() + interval '1 day')`, mcptoken.HashSecret(secret))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.mcp_tokens
			WHERE token_hash = $1`, mcptoken.HashSecret(secret))
	})
	_, err = tokens.Validate(ctx, secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
}

func TestIdentityForGrant_OperatorTokenIsNoAgent(t *testing.T) {
	pool := livePool(t)
	s := NewStore(pool)
	id, ok, err := IdentityForGrant(context.Background(), s,
		mcptoken.Grant{Kind: mcptoken.KindOperator, TokenID: "t"})
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, "", id.Principal.ID)
	_, _, err = IdentityForGrant(context.Background(), s,
		mcptoken.Grant{Kind: mcptoken.KindAgent, TokenID: "t"})
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized, "an agent grant without a principal")
	_, _, err = IdentityForGrant(context.Background(), s, mcptoken.Grant{
		Kind: mcptoken.KindAgent, PrincipalID: "agp_aaaaaaaaaaaaaaaaaaaa"})
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized, "a deleted principal")
	_, _, err = IdentityForGrant(context.Background(), NewStore(nil), mcptoken.Grant{
		Kind: mcptoken.KindAgent, PrincipalID: "agp_aaaaaaaaaaaaaaaaaaaa"})
	require.ErrorIs(t, err, ErrUnavailable, "storage failure is not unauthorized")
}
