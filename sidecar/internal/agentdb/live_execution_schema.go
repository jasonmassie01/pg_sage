package agentdb

func init() {
	schemaStatements = append(schemaStatements, liveExecutionSchemaStatements...)
}

var liveExecutionSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS sage.agent_db_live_plans (
		plan_hash text PRIMARY KEY,
		deployment_id text NOT NULL
			REFERENCES sage.agent_db_deployments(deployment_id) ON DELETE CASCADE,
		payload jsonb NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_live_estimates (
		estimate_id text PRIMARY KEY,
		plan_hash text NOT NULL
			REFERENCES sage.agent_db_live_plans(plan_hash) ON DELETE CASCADE,
		deployment_id text NOT NULL
			REFERENCES sage.agent_db_deployments(deployment_id) ON DELETE CASCADE,
		payload jsonb NOT NULL,
		expires_at timestamptz NOT NULL,
		superseded_at timestamptz,
		consumed_at timestamptz,
		created_at timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_live_authorizations (
		authorization_id text PRIMARY KEY,
		estimate_id text NOT NULL
			REFERENCES sage.agent_db_live_estimates(estimate_id) ON DELETE CASCADE,
		plan_hash text NOT NULL
			REFERENCES sage.agent_db_live_plans(plan_hash) ON DELETE CASCADE,
		deployment_id text NOT NULL
			REFERENCES sage.agent_db_deployments(deployment_id) ON DELETE CASCADE,
		operation text NOT NULL,
		requester_id text NOT NULL,
		idempotency_key text NOT NULL,
		payload jsonb NOT NULL,
		expires_at timestamptz NOT NULL,
		revoked_at timestamptz,
		consumed_at timestamptz,
		created_at timestamptz NOT NULL DEFAULT now(),
		UNIQUE (deployment_id, operation, idempotency_key)
	)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_live_receipts (
		authorization_id text PRIMARY KEY
			REFERENCES sage.agent_db_live_authorizations(authorization_id)
			ON DELETE CASCADE,
		idempotency_key text NOT NULL,
		plan_hash text NOT NULL,
		payload jsonb NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_live_estimates_deployment
		ON sage.agent_db_live_estimates(deployment_id, expires_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_live_authorizations_deployment
		ON sage.agent_db_live_authorizations(deployment_id, expires_at DESC)`,
}
