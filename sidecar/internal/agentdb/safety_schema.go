package agentdb

func init() {
	schemaStatements = append(schemaStatements, safetySchemaStatements...)
}

// safetySchemaStatements add the columns and tables used by the 2026-09-26
// lifecycle-safety fixes (heartbeat isolation, durable teardown, ambiguous
// create tracking, single-use attributed requests and tenant-bound agent
// tokens).
var safetySchemaStatements = []string{
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS agent_status text NOT NULL DEFAULT ''`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS teardown_blocked_reason text NOT NULL DEFAULT ''`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS teardown_blocked_at timestamptz`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS create_operation_id text NOT NULL DEFAULT ''`,
	`ALTER TABLE sage.agent_db_requests
		ADD COLUMN IF NOT EXISTS consumed_deployment_id text NOT NULL DEFAULT ''`,
	// D4: who decided a request and who consumed it, and when.
	`ALTER TABLE sage.agent_db_requests
		ADD COLUMN IF NOT EXISTS decided_by text NOT NULL DEFAULT ''`,
	`ALTER TABLE sage.agent_db_requests
		ADD COLUMN IF NOT EXISTS decided_at timestamptz`,
	`ALTER TABLE sage.agent_db_requests
		ADD COLUMN IF NOT EXISTS consumed_by text NOT NULL DEFAULT ''`,
	`ALTER TABLE sage.agent_db_requests
		ADD COLUMN IF NOT EXISTS consumed_at timestamptz`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_agent_tokens (
		token_id text PRIMARY KEY,
		tenant_id text NOT NULL,
		agent_id text NOT NULL,
		token_hash text NOT NULL UNIQUE,
		status text NOT NULL DEFAULT 'active',
		created_by text NOT NULL DEFAULT '',
		expires_at timestamptz NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now(),
		last_used_at timestamptz,
		revoked_at timestamptz
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_agent_tokens_agent
		ON sage.agent_db_agent_tokens(agent_id, status)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_schema_version (
		singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
		version integer NOT NULL,
		updated_at timestamptz NOT NULL DEFAULT now()
	)`,
}
