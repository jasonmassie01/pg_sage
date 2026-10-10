package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Agent governance startup: legacy agent tokens are bound at start
// (G1-11), the readiness line names why governance is posture-only, and
// mcp.stdio_principal resolves to the stdio client's identity.

func agentWiringPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err := schema.Bootstrap(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAgentGovernanceStartup_BindsLegacyTokens(t *testing.T) {
	pool := agentWiringPool(t)
	ctx := context.Background()
	secret := mcptoken.SecretPrefix + strings.Repeat("W", 43)
	name := fmt.Sprintf("Wiring Bot %d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO sage.mcp_tokens (name, kind, scopes,
		databases, token_hash, prefix, created_by, expires_at) VALUES ($1, 'agent',
		'{read}', '{*}', $2, 'pgs_mcp_WWWW', 'legacy', now() + interval '1 day')`,
		name, mcptoken.HashSecret(secret)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.mcp_tokens WHERE "+
			"token_hash = $1", mcptoken.HashSecret(secret))
	})
	out := captureStderr(t, func() {
		runAgentGovernanceStartup(ctx, pool, &config.Config{})
	})
	if !strings.Contains(out, "profile legacy, no sponsor") ||
		!strings.Contains(out, name) {
		t.Fatalf("migration not logged: %q", out)
	}
	if !strings.Contains(out, "posture-only: set encryption_key") {
		t.Fatalf("readiness line missing: %q", out)
	}
	grant, err := mcptoken.NewStore(pool).Validate(ctx, secret)
	if err != nil || !agentguard.ValidID(grant.PrincipalID) {
		t.Fatalf("token not bound: %+v %v", grant, err)
	}
}

func TestAgentGovernanceStartup_SelfCheckLine(t *testing.T) {
	pool := agentWiringPool(t)
	out := captureStderr(t, func() {
		logAgentGovernanceReadiness(context.Background(), pool,
			&config.Config{EncryptionKey: "k"})
	})
	// The test server's role is superuser: governance stays posture-only.
	if !strings.Contains(out, "posture-only") || !strings.Contains(out, "is superuser") {
		t.Fatalf("self-check line: %q", out)
	}
}

func TestAgentGovernanceStartup_NoPoolIsAnError(t *testing.T) {
	out := captureStderr(t, func() {
		runAgentGovernanceStartup(context.Background(), nil, nil)
	})
	if !strings.Contains(out, "[ERROR] [agents]") {
		t.Fatalf("no error line: %q", out)
	}
}

func TestStdioAgentIdentity(t *testing.T) {
	pool := agentWiringPool(t)
	ctx := context.Background()
	if _, ok, err := stdioAgentIdentity(ctx, pool, ""); ok || err != nil {
		t.Fatalf("empty name: %v %v", ok, err)
	}
	if _, _, err := stdioAgentIdentity(ctx, pool, "no-such-agent-here"); err == nil ||
		!strings.Contains(err.Error(), "names no agent") {
		t.Fatalf("missing: %v", err)
	}
	store := agentguard.NewStore(pool)
	p, err := store.Create(ctx, agentguard.CreateRequest{
		Name: fmt.Sprintf("stdio-%d", time.Now().UnixNano()), Profile: "readonly-analyst",
		CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	id, ok, err := stdioAgentIdentity(ctx, pool, p.Name)
	if err != nil || !ok || id.Principal.ID != p.ID {
		t.Fatalf("resolve: %+v %v %v", id, ok, err)
	}
	if _, err := store.SetStatus(ctx, p.ID, agentguard.StatusRetired, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := stdioAgentIdentity(ctx, pool, p.Name); err == nil ||
		!strings.Contains(err.Error(), "retired") {
		t.Fatalf("retired: %v", err)
	}
}
