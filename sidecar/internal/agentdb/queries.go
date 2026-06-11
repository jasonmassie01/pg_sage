package agentdb

const selectRequestsSQL = `/* pg_sage */
	SELECT request_id, tenant_id, agent_id, owner_id, run_id, purpose,
		requested_isolation_type, database_name, provider, policy_decision, status,
		idempotency_key, body_hash, budget_usd, backup_required, policy_reasons,
		created_at, updated_at
	FROM sage.agent_db_requests`

const insertRequestSQL = `/* pg_sage */
	INSERT INTO sage.agent_db_requests (
		request_id, tenant_id, agent_id, owner_id, run_id, purpose,
		requested_isolation_type, database_name, provider, policy_decision, status,
		idempotency_key, body_hash, budget_usd, backup_required, policy_reasons
	)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		$15, $16::jsonb)
	RETURNING request_id, tenant_id, agent_id, owner_id, run_id, purpose,
		requested_isolation_type, database_name, provider, policy_decision, status,
		idempotency_key, body_hash, budget_usd, backup_required, policy_reasons,
		created_at, updated_at`

const selectDeploymentsSQL = `/* pg_sage */
	SELECT deployment_id, tenant_id, agent_id, run_id, database_name, status,
		safety_mode, isolation_type, schema_name, provider, provisioning_level,
		size_profile_id, provisioning_status, provider_resource_id, secret_ref,
		secret_ref_provider, secret_ref_expires_at, live_mode, budget_usd,
		backup_required, created_at, updated_at, last_ping_at, lease_expires_at,
		metadata, provisioning_plan, connection_info
	FROM sage.agent_db_deployments`

const registerSQL = `/* pg_sage */
	INSERT INTO sage.agent_db_deployments (
		deployment_id, tenant_id, agent_id, run_id, database_name, safety_mode,
		isolation_type, schema_name, provider, provisioning_level,
		size_profile_id, provisioning_status, provider_resource_id, secret_ref,
		secret_ref_provider, secret_ref_expires_at, live_mode, budget_usd,
		backup_required, lease_expires_at, metadata, provisioning_plan,
		connection_info
	)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
		$11, $12, $13, $14, $15, $16, $17, $18, $19,
		now()+make_interval(secs => $20), $21::jsonb, $22::jsonb, $23::jsonb)
	ON CONFLICT (deployment_id) DO UPDATE
	SET tenant_id=EXCLUDED.tenant_id,
		agent_id=EXCLUDED.agent_id,
		run_id=EXCLUDED.run_id,
		database_name=EXCLUDED.database_name,
		safety_mode=EXCLUDED.safety_mode,
		isolation_type=EXCLUDED.isolation_type,
		schema_name=EXCLUDED.schema_name,
		provider=EXCLUDED.provider,
		provisioning_level=EXCLUDED.provisioning_level,
		size_profile_id=EXCLUDED.size_profile_id,
		provisioning_status=EXCLUDED.provisioning_status,
		provider_resource_id=EXCLUDED.provider_resource_id,
		secret_ref=EXCLUDED.secret_ref,
		secret_ref_provider=EXCLUDED.secret_ref_provider,
		secret_ref_expires_at=EXCLUDED.secret_ref_expires_at,
		live_mode=EXCLUDED.live_mode,
		budget_usd=EXCLUDED.budget_usd,
		backup_required=EXCLUDED.backup_required,
		lease_expires_at=EXCLUDED.lease_expires_at,
		metadata=EXCLUDED.metadata,
		provisioning_plan=EXCLUDED.provisioning_plan,
		connection_info=EXCLUDED.connection_info,
		status='active',
		updated_at=now()
	RETURNING deployment_id, tenant_id, agent_id, run_id, database_name, status,
		safety_mode, isolation_type, schema_name, provider, provisioning_level,
		size_profile_id, provisioning_status, provider_resource_id, secret_ref,
		secret_ref_provider, secret_ref_expires_at, live_mode, budget_usd,
		backup_required, created_at, updated_at, last_ping_at, lease_expires_at,
		metadata, provisioning_plan, connection_info`
