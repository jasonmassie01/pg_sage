package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/config"
)

// agentGovernanceStartupTimeout bounds the startup migration and checks.
const agentGovernanceStartupTimeout = 30 * time.Second

// startAgentCoreStartup runs agent governance's startup work on the control
// database in the background: it binds legacy agent tokens to principals
// (G1-11) and self-checks pg_sage's own role (G1-01). Neither refuses the
// start: an unbound agent token fails closed until the migration succeeds,
// and a failed self-check leaves agent governance posture-only.
func startAgentCoreStartup(ctx context.Context, pool *pgxpool.Pool, c *config.Config) {
	go runAgentGovernanceStartup(ctx, pool, c)
}

func runAgentGovernanceStartup(ctx context.Context, pool *pgxpool.Pool, c *config.Config) {
	sctx, cancel := context.WithTimeout(ctx, agentGovernanceStartupTimeout)
	defer cancel()
	res, err := agentguard.MigrateLegacyTokens(sctx, pool)
	if err != nil {
		logError("agents", "binding legacy agent tokens failed; they stay refused "+
			"until it succeeds at the next start: %v", err)
	}
	for _, m := range res.Migrated {
		logWarn("agents", "agent token %q is now agent %q (profile legacy, no sponsor): "+
			"reads work, proposals queue for a human; assign a sponsor in Agents",
			m.TokenName, m.Principal)
	}
	if res.AgentDBTokens > 0 {
		logWarn("agents", "%d live tokens of the removed AgentDB provisioner remain in "+
			"sage.agent_db_agent_tokens; nothing accepts them", res.AgentDBTokens)
	}
	logAgentGovernanceReadiness(sctx, pool, c)
}

// logAgentGovernanceReadiness says in one line why agent governance is
// posture-only, when it is (§6.4: no encryption key; §6.6, AP-14: pg_sage's
// role is superuser, holds BYPASSRLS, inherits an agent role or lacks
// CREATEROLE).
func logAgentGovernanceReadiness(ctx context.Context, pool *pgxpool.Pool, c *config.Config) {
	if c == nil || c.EncryptionKey == "" {
		logInfo("agents", "agent governance is posture-only: set encryption_key to "+
			"store agent broker credentials")
		return
	}
	self, err := agentguard.SelfCheck(ctx, pool)
	if err != nil {
		logError("agents", "self-check of pg_sage's role failed: %v", err)
		return
	}
	if err := self.Err(); err != nil {
		logWarn("agents", "agent governance is posture-only: %v", err)
	}
}
