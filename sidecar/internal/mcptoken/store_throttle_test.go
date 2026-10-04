package mcptoken_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// last_used_at is written at most once a minute per token, so a busy agent
// does not turn every MCP call into a row update.

func lastUsed(t *testing.T, pool *pgxpool.Pool, id string) time.Time {
	t.Helper()
	var at *time.Time
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT last_used_at FROM sage.mcp_tokens WHERE id::text = $1`, id).Scan(&at))
	require.NotNil(t, at, "last_used_at must be set after a validation")
	return *at
}

func setLastUsedAgo(t *testing.T, pool *pgxpool.Pool, id string, ago time.Duration) time.Time {
	t.Helper()
	var at time.Time
	require.NoError(t, pool.QueryRow(context.Background(), `UPDATE sage.mcp_tokens
		SET last_used_at = now() - make_interval(secs => $2)
		WHERE id::text = $1 RETURNING last_used_at`, id, ago.Seconds()).Scan(&at))
	return at
}

func TestValidateThrottlesLastUsedWrites(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	ctx := context.Background()
	tok := mustCreate(t, s, agentReq(uniq("throttle")))

	_, err := s.Validate(ctx, tok.Secret)
	require.NoError(t, err)
	first := lastUsed(t, pool, tok.ID)
	_, err = s.Validate(ctx, tok.Secret)
	require.NoError(t, err)
	require.True(t, first.Equal(lastUsed(t, pool, tok.ID)),
		"a second use within a minute must not rewrite last_used_at")

	recent := setLastUsedAgo(t, pool, tok.ID, 50*time.Second)
	_, err = s.Validate(ctx, tok.Secret)
	require.NoError(t, err)
	require.True(t, recent.Equal(lastUsed(t, pool, tok.ID)), "50s ago is inside the window")

	stale := setLastUsedAgo(t, pool, tok.ID, 2*time.Minute)
	grant, err := s.Validate(ctx, tok.Secret)
	require.NoError(t, err)
	require.Equal(t, tok.ID, grant.TokenID)
	refreshed := lastUsed(t, pool, tok.ID)
	require.True(t, refreshed.After(stale), "a use after a minute must refresh last_used_at")
	require.True(t, refreshed.Sub(stale) > time.Minute+30*time.Second,
		"refreshed to now, got %s after the stale value", refreshed.Sub(stale))
}

func TestValidateRefusedTokenDoesNotTouchLastUsed(t *testing.T) {
	pool := livePool(t)
	s := mcptoken.NewStore(pool)
	ctx := context.Background()
	tok := mustCreate(t, s, agentReq(uniq("throttle-revoked")))
	stale := setLastUsedAgo(t, pool, tok.ID, 10*time.Minute)
	_, err := s.Revoke(ctx, tok.ID, "admin@example.com")
	require.NoError(t, err)

	_, err = s.Validate(ctx, tok.Secret)
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
	require.True(t, stale.Equal(lastUsed(t, pool, tok.ID)),
		"a refused validation must not record a use")
}
