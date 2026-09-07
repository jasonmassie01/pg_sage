package agentdb

func init() {
	schemaStatements = append(schemaStatements, monitoringSchemaStatements...)
}

var monitoringSchemaStatements = []string{
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS monitoring_mode text NOT NULL DEFAULT 'adaptive'`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS execution_mode text NOT NULL DEFAULT 'manual'`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS wake_idle_allowed boolean NOT NULL DEFAULT false`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_monitoring_policies (
		scope_type text NOT NULL,
		scope_id text NOT NULL,
		max_concurrency integer NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now(),
		updated_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (scope_type, scope_id)
	)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_monitoring_state (
		physical_target_key text PRIMARY KEY,
		deployment_id text NOT NULL,
		tenant_id text NOT NULL,
		provider text NOT NULL,
		next_due_at timestamptz NOT NULL,
		last_scheduled_at timestamptz,
		last_completed_at timestamptz,
		updated_at timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_monitoring_state_due
		ON sage.agent_db_monitoring_state(next_due_at)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_monitoring_work (
		work_id text PRIMARY KEY,
		deployment_id text NOT NULL,
		tenant_id text NOT NULL,
		provider text NOT NULL,
		physical_target_key text NOT NULL,
		tier text NOT NULL,
		status text NOT NULL DEFAULT 'queued',
		next_due_at timestamptz NOT NULL,
		claim_id text NOT NULL DEFAULT '',
		claim_owner text NOT NULL DEFAULT '',
		claim_expires_at timestamptz,
		revoked_at timestamptz,
		created_at timestamptz NOT NULL DEFAULT now(),
		updated_at timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_monitoring_work_due
		ON sage.agent_db_monitoring_work(next_due_at, work_id)
		WHERE status IN ('queued', 'claimed') AND revoked_at IS NULL`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_monitoring_work_claims
		ON sage.agent_db_monitoring_work(claim_expires_at, provider, tenant_id)
		WHERE status = 'claimed'`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_monitoring_work_deployment
		ON sage.agent_db_monitoring_work(deployment_id, status)`,
}
