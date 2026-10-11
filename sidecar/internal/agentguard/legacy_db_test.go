package agentguard

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/decommission"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestSlug_Rule(t *testing.T) {
	cases := map[string]string{
		"Claude Code (prod)": "claude-code--prod-",
		"ci-agent":           "ci-agent",
		"CI_Agent.v2":        "ci-agent-v2",
		"1password":          "agent-1password",
		"x":                  "agent-x",
		"":                   "agent-",
		"-dash":              "agent--dash",
		"Ünïcode bot":        "-n-code-bot",
	}
	for in, want := range cases {
		got := Slug(in)
		if in == "Ünïcode bot" {
			want = "agent-" + want
		}
		require.Equal(t, want, got, "slug of %q", in)
		require.True(t, ValidName(got), "slug %q of %q is a valid name", got, in)
	}
	long := Slug(strings.Repeat("a", 100))
	require.Len(t, long, 54)
	require.True(t, ValidName(long+"-abcd"), "room for the collision suffix")
}

// legacyToken inserts a pre-G1 agent token (no principal) and returns its
// secret.
func insertLegacyToken(t *testing.T, pool *pgxpool.Pool, name, state string) string {
	t.Helper()
	buf := make([]byte, 32)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	secret := mcptoken.SecretPrefix + base64.RawURLEncoding.EncodeToString(buf)
	revoked, expires := "NULL", "now() + interval '1 day'"
	switch state {
	case "revoked":
		revoked = "now()"
	case "expired":
		expires = "now() - interval '1 second'"
	}
	_, err = pool.Exec(context.Background(), `INSERT INTO sage.mcp_tokens (name, kind,
		scopes, databases, token_hash, prefix, created_by, created_at, expires_at, revoked_at)
		VALUES ($1, 'agent', '{read,propose}', '{*}', $2, $3, 'legacy',
		now() - interval '2 days', `+expires+`, `+revoked+`)`,
		name, mcptoken.HashSecret(secret), secret[:12])
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.mcp_tokens
			WHERE token_hash = $1`, mcptoken.HashSecret(secret))
	})
	return secret
}

func TestMigrateLegacyTokens_G111(t *testing.T) {
	pool := livePool(t)
	ctx := context.Background()
	s, tokens := NewStore(pool), mcptoken.NewStore(pool)
	tag := strings.ToLower(uniqName("Legacy"))
	names := []string{tag + " Claude (prod)", tag + " Claude (prod)", "1" + tag,
		strings.Repeat("Z", 70) + tag}
	secrets := map[string]string{}
	for i, n := range names {
		secrets[n+string(rune('a'+i))] = insertLegacyToken(t, pool, n, "live")
	}
	revoked := insertLegacyToken(t, pool, tag+"-revoked", "revoked")
	expired := insertLegacyToken(t, pool, tag+"-expired", "expired")
	_, err := tokens.Validate(ctx, secrets[names[0]+"a"])
	require.ErrorIs(t, err, mcptoken.ErrUnauthorized, "unbound tokens fail closed")

	res, err := MigrateLegacyTokens(ctx, pool)
	require.NoError(t, err)
	mine := map[string]bool{}
	for _, m := range res.Migrated {
		if strings.Contains(strings.ToLower(m.TokenName), tag) {
			require.True(t, ValidName(m.Principal), "slug %q", m.Principal)
			require.False(t, mine[m.Principal], "slugs are unique: %q", m.Principal)
			mine[m.Principal] = true
		}
	}
	require.Len(t, mine, 4, "the four live tokens; revoked and expired stay unbound")
	for _, secret := range secrets {
		id, _, err := Authenticate(ctx, s, tokens, secret)
		require.NoError(t, err, "read tools keep working")
		p := id.Principal
		require.Equal(t, LegacyProfile, p.Profile)
		require.Equal(t, EnvProd, p.EnvCeiling)
		require.Nil(t, p.SponsorUserID)
		require.True(t, ToolAccess(p, ToolRead).Allowed)
		require.Equal(t, Access{Allowed: true, MaxLevel: 2}, ToolAccess(p, ToolPropose),
			"propose tools queue for a human")
		require.Equal(t, ReasonUnsponsored, ToolAccess(p, ToolAgent).Reason)
	}
	for _, dead := range []string{revoked, expired} {
		_, err = tokens.Validate(ctx, dead)
		require.ErrorIs(t, err, mcptoken.ErrUnauthorized)
	}
	again, err := MigrateLegacyTokens(ctx, pool)
	require.NoError(t, err)
	for _, m := range again.Migrated {
		require.False(t, strings.Contains(strings.ToLower(m.TokenName), tag),
			"idempotent: %+v", m)
	}
}

func TestMigrateLegacyTokens_ConcurrentSidecarsMigrateOnce(t *testing.T) {
	pool := livePool(t)
	tag := strings.ToLower(uniqName("race"))
	for i := 0; i < 5; i++ {
		insertLegacyToken(t, pool, tag, "live")
	}
	var wg sync.WaitGroup
	results := make([]LegacyResult, 3)
	errs := make([]error, 3)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = MigrateLegacyTokens(context.Background(), pool)
		}(i)
	}
	wg.Wait()
	total := 0
	for i := range results {
		require.NoError(t, errs[i])
		for _, m := range results[i].Migrated {
			if strings.HasPrefix(m.TokenName, tag) {
				total++
			}
		}
	}
	require.Equal(t, 5, total, "each token is migrated exactly once")
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(DISTINCT
		principal_id) FROM sage.mcp_tokens WHERE name = $1`, tag).Scan(&n))
	require.Equal(t, 5, n)
}

func TestMigrateLegacyTokens_ReportsRemovedProvisionerTokens(t *testing.T) {
	pool := livePool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS sage.`+decommission.LegacyTokensTable+` (
		token_id text PRIMARY KEY, tenant_id text NOT NULL, agent_id text NOT NULL,
		token_hash text NOT NULL UNIQUE, status text NOT NULL DEFAULT 'active',
		created_by text NOT NULL DEFAULT '', expires_at timestamptz NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now(), last_used_at timestamptz,
		revoked_at timestamptz)`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS sage."+
			decommission.LegacyTokensTable)
	})
	_, err = pool.Exec(ctx, `INSERT INTO sage.`+decommission.LegacyTokensTable+` (token_id, tenant_id,
		agent_id, token_hash, expires_at, revoked_at) VALUES
		('t1', 'acme', 'a1', 'h1', now() + interval '1 day', NULL),
		('t2', 'acme', 'a1', 'h2', now() + interval '1 day', now()),
		('t3', 'acme', 'a1', 'h3', now() - interval '1 day', NULL)`)
	require.NoError(t, err)
	res, err := MigrateLegacyTokens(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, 1, res.RemovedProvisionerTokens,
		"only the live token of the removed provisioner is reported")
	_, err = MigrateLegacyTokens(ctx, nil)
	require.ErrorIs(t, err, ErrUnavailable)
}
