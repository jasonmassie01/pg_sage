package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// observability-08 on the real router and the live control database: the
// token store, its scopes and revocation are real.

func metricsToken(t *testing.T, pool *pgxpool.Pool, scopes ...string) mcptoken.Token {
	t.Helper()
	owner := tokenRouteUser(t, pool, "operator")
	tok, err := mcptoken.NewStore(pool).Create(context.Background(), mcptoken.CreateRequest{
		Name: fmt.Sprintf("obs08-%d", time.Now().UnixNano()), Kind: mcptoken.KindOperator,
		Scopes: scopes, Databases: []string{"*"}, ExpiresIn: time.Hour,
		OwnerUserID: owner.ID, CreatedBy: "test@example.com",
	})
	require.NoError(t, err)
	return tok
}

func TestMetricsRouteLiveTokens(t *testing.T) {
	pool := surfacePool(t)
	h := newTestRouter(nil, config.DefaultConfig(), pool, nil, nil, nil,
		&RuntimeDeps{Metrics: &MetricsRoute{Handler: metricsStub()}})

	scrapeTok := metricsToken(t, pool, mcptoken.ScopeMetrics)
	requireExposition(t, scrape(h, "Bearer "+scrapeTok.Secret))

	readTok := metricsToken(t, pool, mcptoken.ScopeRead)
	requireRefused(t, scrape(h, "Bearer "+readTok.Secret), http.StatusForbidden)

	wrong := mcptoken.SecretPrefix + "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	requireRefused(t, scrape(h, "Bearer "+wrong), http.StatusUnauthorized)
	requireRefused(t, scrape(h, ""), http.StatusUnauthorized)

	_, err := mcptoken.NewStore(pool).Revoke(context.Background(), scrapeTok.ID,
		"test@example.com", mcptoken.Audit{})
	require.NoError(t, err)
	requireRefused(t, scrape(h, "Bearer "+scrapeTok.Secret), http.StatusUnauthorized)
}

// A scrape credential is refused by the MCP endpoint: it grants no tool.
func TestMetricsTokenIsRefusedByMCP(t *testing.T) {
	pool := surfacePool(t)
	tok := metricsToken(t, pool, mcptoken.ScopeMetrics)
	p, _, err := tokenPrincipal(context.Background(), mcptoken.NewStore(pool), tok.Secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
	require.Equal(t, "", p.TokenID)
}
