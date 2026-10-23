package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// MCP over HTTP is token-only: tests act through MCP with tokens, the way
// MCP clients do. A "viewer" is a person's read-only token; an
// "operator" a person's token with every scope.

func mcpRoleToken(t *testing.T, pool *pgxpool.Pool, role string) mcptoken.Token {
	t.Helper()
	scopes := []string{"read", "propose", "approve"}
	if role == "viewer" {
		scopes = []string{"read"}
	}
	owner := tokenRouteUser(t, pool, "operator")
	tok, err := mcptoken.NewStore(pool).Create(context.Background(), mcptoken.CreateRequest{
		Name: fmt.Sprintf("test-%s-%d", role, time.Now().UnixNano()),
		Kind: mcptoken.KindOperator, Scopes: scopes, Databases: []string{"*"},
		ExpiresIn: time.Hour, OwnerUserID: owner.ID, CreatedBy: "test@example.com",
	})
	require.NoError(t, err)
	return tok
}

// mintAgentToken creates a sponsored agent principal and an agent token
// for it (from G1 every agent token acts for a principal, §6.4).
func mintAgentToken(t *testing.T, pool *pgxpool.Pool, databases []string) mcptoken.Token {
	t.Helper()
	ctx := context.Background()
	sponsor := tokenRouteUser(t, pool, "admin")
	principals := agentguard.NewStore(pool)
	p, err := principals.Create(ctx, agentguard.CreateRequest{
		Name:    fmt.Sprintf("api-agent-%d", time.Now().UnixNano()),
		Profile: "readonly-analyst", EnvCeiling: agentguard.EnvProd,
		SponsorUserID: &sponsor.ID, CreatedBy: "admin@example.com"})
	require.NoError(t, err)
	tok, err := agentguard.IssueToken(ctx, principals, mcptoken.NewStore(pool), p.ID,
		agentguard.TokenRequest{Name: "bearer-agent-" + p.Name,
			Scopes: []string{"read", "propose"}, Databases: databases,
			ExpiresIn: 24 * time.Hour, CreatedBy: "admin@example.com"})
	require.NoError(t, err)
	return tok
}

// withBearer sends every request through h with the token, like an MCP
// client configured with it.
func withBearer(h http.Handler, secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if secret != "" {
			r.Header.Set("Authorization", "Bearer "+secret)
		}
		h.ServeHTTP(w, r)
	})
}
