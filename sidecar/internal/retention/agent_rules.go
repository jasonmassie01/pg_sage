package retention

import "github.com/pg-sage/sidecar/internal/config"

// agentRules age out the append-only agent_db_* rows (perf storage phase:
// the 18 tables, created on first use, had no retention at all). Rows that
// back a live object, a ledger or an audit trail are exempt (exemptions.go)
// or kept by a predicate here. Every rule is optional: before the agent
// API is first used the tables do not exist.
func agentRules(r config.RetentionConfig) []purgeRule {
	return []purgeRule{
		// Heartbeats have no reader; a deployment keeps its newest one.
		{table: "agent_db_pings", timeCol: "created_at", days: r.SnapshotsDays,
			optional: true, extra: keepNewestPerDeployment("agent_db_pings")},
		{table: "agent_db_ping_token_failures", timeCol: "created_at", days: r.ActionsDays,
			optional: true},
		// Tokens age from when they stopped working (revoked or expired).
		{table: "agent_db_ping_tokens", timeCol: "COALESCE(revoked_at, expires_at)",
			days: r.ActionsDays, optional: true},
		{table: "agent_db_agent_tokens", timeCol: "COALESCE(revoked_at, expires_at)",
			days: r.ActionsDays, optional: true},
		// Command output of provisioning runs; the latest one is the
		// deployment's current story.
		{table: "agent_db_provision_attempts", timeCol: "created_at", days: r.ActionsDays,
			optional: true, extra: keepNewestPerDeployment("agent_db_provision_attempts")},
		// An estimate an authorization was issued on is evidence of a live
		// operation (deleting it would cascade to the authorization).
		{table: "agent_db_live_estimates", timeCol: "expires_at", days: r.ActionsDays,
			optional: true, extra: `AND NOT EXISTS (SELECT 1 FROM
			sage.agent_db_live_authorizations a
			WHERE a.estimate_id = agent_db_live_estimates.estimate_id)`},
		// Monitoring work of deleted deployments (live work is upserted in
		// place, one row per target and tier).
		{table: "agent_db_monitoring_work", timeCol: "revoked_at", days: r.ActionsDays,
			optional: true, extra: "AND status = 'revoked'"},
	}
}

// keepNewestPerDeployment keeps the newest row of each deployment.
func keepNewestPerDeployment(table string) string {
	return `AND EXISTS (SELECT 1 FROM sage.` + table + ` n
	    WHERE n.deployment_id = ` + table + `.deployment_id
	      AND n.created_at > ` + table + `.created_at)`
}
