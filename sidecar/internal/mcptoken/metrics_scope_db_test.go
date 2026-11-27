package mcptoken_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// observability-08 against the live control database: a metrics token
// authorizes only a scrape, and every MCP-side Validate refuses it.

func metricsReq(name string, owner int) mcptoken.CreateRequest {
	req := operatorReq(name, owner)
	req.Scopes = []string{mcptoken.ScopeMetrics}
	return req
}

func TestMetricsTokenValidatesOnlyForScrapes(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	owner := createUser(t, pool, auth.RoleOperator)
	tok := mustCreate(t, s, metricsReq(uniq("scrape"), owner))
	require.Equal(t, []string{"metrics"}, tok.Scopes)
	require.Equal(t, mcptoken.KindOperator, tok.Kind)

	grant, err := s.ValidateMetrics(context.Background(), tok.Secret)
	require.NoError(t, err)
	require.Equal(t, tok.ID, grant.TokenID)
	require.Equal(t, []string{"metrics"}, grant.Scopes)
	require.Equal(t, owner, grant.OwnerUserID)

	// A scrape credential is not an MCP credential.
	mcpGrant, err := s.Validate(context.Background(), tok.Secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
	require.Equal(t, "", mcpGrant.TokenID)
}

func TestMetricsValidateRefusesReadTokens(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	owner := createUser(t, pool, auth.RoleAdmin)
	read := operatorReq(uniq("read-only"), owner)
	read.Scopes = []string{"read"}
	tok := mustCreate(t, s, read)
	g, err := s.ValidateMetrics(context.Background(), tok.Secret)
	require.ErrorIs(t, err, mcptoken.ErrScopeMissing)
	require.False(t, errors.Is(err, mcptoken.ErrUnauthorized),
		"a valid token lacking the scope is a 403, not a 401")
	require.Equal(t, "", g.TokenID)

	agent := mustCreate(t, s, agentReq(uniq("agent-scrape")))
	_, err = s.ValidateMetrics(context.Background(), agent.Secret)
	require.ErrorIs(t, err, mcptoken.ErrScopeMissing)
}

// State transitions: an owner demoted to viewer keeps the read-level
// metrics scope; a deactivated owner and revocation end the scrapes.
func TestMetricsTokenFollowsRevocationAndOwner(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	ctx := context.Background()

	owner := createUser(t, pool, auth.RoleOperator)
	tok := mustCreate(t, s, metricsReq(uniq("demote"), owner))
	setRole(t, pool, owner, auth.RoleViewer)
	grant, err := s.ValidateMetrics(ctx, tok.Secret)
	require.NoError(t, err, "metrics is read-level: a viewer owner keeps it")
	require.Equal(t, auth.RoleViewer, grant.OwnerRole)
	require.Equal(t, []string{"metrics"}, grant.Scopes)
	setRole(t, pool, owner, "robot")
	_, err = s.ValidateMetrics(ctx, tok.Secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized, "an unknown role grants nothing")
	setRole(t, pool, owner, auth.RoleAdmin)
	_, err = s.ValidateMetrics(ctx, tok.Secret)
	require.NoError(t, err)

	setDisabled(t, pool, owner, true)
	_, err = s.ValidateMetrics(ctx, tok.Secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
	setDisabled(t, pool, owner, false)

	_, err = s.Revoke(ctx, tok.ID, "admin@example.com", mcptoken.Audit{})
	require.NoError(t, err)
	_, err = s.ValidateMetrics(ctx, tok.Secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
}

// The table CHECK repeats the store's rule (defense in depth): metrics is
// alone and only on an operator token.
func TestDatabaseCheckBoundsTheMetricsScope(t *testing.T) {
	pool := livePool(t)
	ctx := context.Background()
	owner := createUser(t, pool, auth.RoleOperator)
	tok := mustCreate(t, mcptoken.NewStore(pool), metricsReq(uniq("check-metrics"), owner))
	for _, scopes := range [][]string{{"read", "metrics"}, {"metrics", "propose"}} {
		_, err := pool.Exec(ctx, `UPDATE sage.mcp_tokens SET scopes = $1
			WHERE id::text = $2`, scopes, tok.ID)
		requireCheckViolation(t, err)
	}
	_, err := pool.Exec(ctx, `UPDATE sage.mcp_tokens SET kind = 'agent',
		owner_user_id = NULL WHERE id::text = $1`, tok.ID)
	requireCheckViolation(t, err)
	_, err = pool.Exec(ctx, `UPDATE sage.mcp_tokens SET scopes = '{bogus}'
		WHERE id::text = $1`, tok.ID)
	requireCheckViolation(t, err)
}

func TestConcurrentMetricsValidate(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	owner := createUser(t, pool, auth.RoleOperator)
	tok := mustCreate(t, s, metricsReq(uniq("concurrent-scrape"), owner))
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ValidateMetrics(context.Background(), tok.Secret)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}
