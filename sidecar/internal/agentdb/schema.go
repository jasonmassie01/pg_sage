package agentdb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5/pgxpool"
)

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Ensure(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("agentdb store unavailable")
	}
	for _, stmt := range schemaStatements {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return s.seedDefaultSizeProfiles(ctx)
}

func (s *Store) ensure(ctx context.Context) error {
	return s.Ensure(ctx)
}

func (s *Store) ProvisionSchema(
	ctx context.Context,
	req RegisterRequest,
) (Deployment, error) {
	normalizeProviderFields(&req)
	if req.Provider != ProviderLocalPostgres || req.ProvisioningLevel != LevelSchema {
		return Deployment{}, ErrInvalid
	}
	req.SchemaName = sanitizeSchemaName(req.SchemaName)
	if req.SchemaName == "" {
		req.SchemaName = "agentdb_" + idFrom(req.TenantID, req.AgentID)
	}
	if err := s.Ensure(ctx); err != nil {
		return Deployment{}, err
	}
	sql := fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", quoteIdent(req.SchemaName))
	if _, err := s.pool.Exec(ctx, sql); err != nil {
		return Deployment{}, err
	}
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	req.Metadata["credential_scope"] = req.SchemaName
	req.ProvisioningStatus = "provisioned"
	req.ConnectionInfo = map[string]any{
		"provider":    req.Provider,
		"schema_name": req.SchemaName,
	}
	return s.Register(ctx, req)
}

func (s *Store) Provision(ctx context.Context, req RegisterRequest) (Deployment, error) {
	normalizeProviderFields(&req)
	if req.Provider == ProviderLocalPostgres {
		switch req.ProvisioningLevel {
		case LevelSchema:
			return s.ProvisionSchema(ctx, req)
		case LevelDatabase:
			return s.provisionLocalDatabase(ctx, req)
		default:
			return Deployment{}, ErrInvalid
		}
	}
	if req.ProvisioningLevel != LevelInstance {
		return Deployment{}, ErrInvalid
	}
	profile, err := s.profileForRequest(ctx, req)
	if err != nil {
		return Deployment{}, err
	}
	plan, err := BuildProvisionPlan(req, profile)
	if err != nil {
		return Deployment{}, err
	}
	req.ProvisioningStatus = "planned"
	req.ProvisioningPlan = planMap(plan)
	return s.Register(ctx, req)
}

func (s *Store) provisionLocalDatabase(
	ctx context.Context,
	req RegisterRequest,
) (Deployment, error) {
	req.DatabaseName = sanitizeDatabaseName(req.DatabaseName)
	if req.DatabaseName == "" {
		req.DatabaseName = "agentdb_" + idFrom(req.TenantID, req.AgentID)
	}
	if err := s.Ensure(ctx); err != nil {
		return Deployment{}, err
	}
	sql := fmt.Sprintf("CREATE DATABASE %s", quoteIdent(req.DatabaseName))
	if _, err := s.pool.Exec(ctx, sql); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return Deployment{}, err
		}
	}
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	req.Metadata["credential_scope"] = req.DatabaseName
	req.ProvisioningStatus = "provisioned"
	req.ConnectionInfo = map[string]any{
		"provider":      req.Provider,
		"database_name": req.DatabaseName,
	}
	return s.Register(ctx, req)
}

func jsonBytes(v map[string]any) []byte {
	if v == nil {
		return []byte(`{}`)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func sanitizeSchemaName(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	var b strings.Builder
	lastUnderscore := false
	for _, r := range v {
		ok := unicode.IsLetter(r) || unicode.IsDigit(r)
		if ok {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return ""
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "agentdb_" + out
	}
	if len(out) > 63 {
		return out[:63]
	}
	return out
}

func sanitizeDatabaseName(v string) string {
	return sanitizeSchemaName(v)
}

func quoteIdent(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}

var schemaStatements = []string{
	`CREATE SCHEMA IF NOT EXISTS sage`,
	`CREATE TABLE IF NOT EXISTS sage.agent_identities (
		agent_id text PRIMARY KEY,
		tenant_id text NOT NULL,
		owner_id text NOT NULL DEFAULT '',
		display_name text NOT NULL DEFAULT '',
		status text NOT NULL DEFAULT 'active',
		metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at timestamptz NOT NULL DEFAULT now(),
		updated_at timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_identities_tenant_status
		ON sage.agent_identities(tenant_id, status)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_requests (
		request_id text PRIMARY KEY,
		tenant_id text NOT NULL,
		agent_id text NOT NULL,
		owner_id text NOT NULL DEFAULT '',
		run_id text NOT NULL DEFAULT '',
		purpose text NOT NULL DEFAULT '',
		requested_isolation_type text NOT NULL DEFAULT 'schema',
		database_name text NOT NULL DEFAULT '',
		policy_decision text NOT NULL DEFAULT 'review',
		status text NOT NULL DEFAULT 'requested',
		idempotency_key text NOT NULL DEFAULT '',
		body_hash text NOT NULL DEFAULT '',
		budget_usd double precision NOT NULL DEFAULT 0,
		backup_required boolean NOT NULL DEFAULT true,
		policy_reasons jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at timestamptz NOT NULL DEFAULT now(),
		updated_at timestamptz NOT NULL DEFAULT now()
	)`,
	`ALTER TABLE sage.agent_db_requests
		ADD COLUMN IF NOT EXISTS budget_usd double precision NOT NULL DEFAULT 0`,
	`ALTER TABLE sage.agent_db_requests
		ADD COLUMN IF NOT EXISTS backup_required boolean NOT NULL DEFAULT true`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_db_requests_idempotency
		ON sage.agent_db_requests(tenant_id, idempotency_key)
		WHERE idempotency_key <> ''`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_deployments (
		deployment_id text PRIMARY KEY,
		tenant_id text NOT NULL,
		agent_id text NOT NULL,
		run_id text NOT NULL DEFAULT '',
		database_name text NOT NULL DEFAULT '',
		status text NOT NULL DEFAULT 'active',
		safety_mode text NOT NULL DEFAULT 'observation',
		isolation_type text NOT NULL DEFAULT 'schema',
		schema_name text NOT NULL DEFAULT '',
		budget_usd double precision NOT NULL DEFAULT 0,
		backup_required boolean NOT NULL DEFAULT true,
		created_at timestamptz NOT NULL DEFAULT now(),
		updated_at timestamptz NOT NULL DEFAULT now(),
		last_ping_at timestamptz,
		lease_expires_at timestamptz,
		metadata jsonb NOT NULL DEFAULT '{}'::jsonb
	)`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS isolation_type text NOT NULL DEFAULT 'schema'`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS schema_name text NOT NULL DEFAULT ''`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS budget_usd double precision NOT NULL DEFAULT 0`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS backup_required boolean NOT NULL DEFAULT true`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS provider text NOT NULL DEFAULT 'local_postgres'`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS provisioning_level text NOT NULL DEFAULT 'schema'`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS size_profile_id text NOT NULL DEFAULT ''`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS provisioning_status text NOT NULL DEFAULT 'registered'`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS provisioning_plan jsonb NOT NULL DEFAULT '{}'::jsonb`,
	`ALTER TABLE sage.agent_db_deployments
		ADD COLUMN IF NOT EXISTS connection_info jsonb NOT NULL DEFAULT '{}'::jsonb`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_deployments_tenant_status
		ON sage.agent_db_deployments(tenant_id, status)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_size_profiles (
		profile_id text PRIMARY KEY,
		provider text NOT NULL,
		provisioning_level text NOT NULL,
		name text NOT NULL,
		description text NOT NULL DEFAULT '',
		cpu double precision NOT NULL DEFAULT 0,
		memory_gb double precision NOT NULL DEFAULT 0,
		storage_gb double precision NOT NULL DEFAULT 0,
		max_connections integer NOT NULL DEFAULT 0,
		monthly_budget_usd double precision NOT NULL DEFAULT 0,
		provider_params jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at timestamptz NOT NULL DEFAULT now(),
		updated_at timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_pings (
		ping_id bigserial PRIMARY KEY,
		deployment_id text NOT NULL
			REFERENCES sage.agent_db_deployments(deployment_id) ON DELETE CASCADE,
		status text NOT NULL,
		metrics jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_ping_tokens (
		token_id text PRIMARY KEY,
		deployment_id text NOT NULL
			REFERENCES sage.agent_db_deployments(deployment_id) ON DELETE CASCADE,
		agent_id text NOT NULL,
		token_hash text NOT NULL UNIQUE,
		scope text NOT NULL DEFAULT 'ping',
		status text NOT NULL DEFAULT 'active',
		expires_at timestamptz NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now(),
		last_used_at timestamptz,
		revoked_at timestamptz,
		rotated_from_token_id text NOT NULL DEFAULT ''
	)`,
	`ALTER TABLE sage.agent_db_ping_tokens
		ADD COLUMN IF NOT EXISTS revoked_at timestamptz`,
	`ALTER TABLE sage.agent_db_ping_tokens
		ADD COLUMN IF NOT EXISTS rotated_from_token_id text NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_ping_tokens_deployment
		ON sage.agent_db_ping_tokens(deployment_id, status)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_ping_token_failures (
		failure_id bigserial PRIMARY KEY,
		deployment_id text NOT NULL DEFAULT '',
		token_hash text NOT NULL DEFAULT '',
		reason text NOT NULL DEFAULT '',
		created_at timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_ping_token_failures_recent
		ON sage.agent_db_ping_token_failures(deployment_id, token_hash, created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_recommendations (
		recommendation_id text PRIMARY KEY,
		deployment_id text NOT NULL
			REFERENCES sage.agent_db_deployments(deployment_id) ON DELETE CASCADE,
		kind text NOT NULL,
		title text NOT NULL,
		detail text NOT NULL DEFAULT '',
		query_fingerprint text NOT NULL DEFAULT '',
		status text NOT NULL DEFAULT 'active',
		feedback jsonb NOT NULL DEFAULT '{}'::jsonb,
		payload jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at timestamptz NOT NULL DEFAULT now(),
		updated_at timestamptz NOT NULL DEFAULT now()
	)`,
	`ALTER TABLE sage.agent_db_recommendations
		ADD COLUMN IF NOT EXISTS query_fingerprint text NOT NULL DEFAULT ''`,
	`ALTER TABLE sage.agent_db_recommendations
		ADD COLUMN IF NOT EXISTS payload jsonb NOT NULL DEFAULT '{}'::jsonb`,
	`ALTER TABLE sage.agent_db_recommendations
		ADD COLUMN IF NOT EXISTS action_type text NOT NULL DEFAULT ''`,
	`ALTER TABLE sage.agent_db_recommendations
		ADD COLUMN IF NOT EXISTS action_risk text NOT NULL DEFAULT 'review'`,
	`ALTER TABLE sage.agent_db_recommendations
		ADD COLUMN IF NOT EXISTS confidence double precision NOT NULL DEFAULT 0`,
	`ALTER TABLE sage.agent_db_recommendations
		ADD COLUMN IF NOT EXISTS agent_instructions jsonb NOT NULL DEFAULT '{}'::jsonb`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_recommendations_deployment
		ON sage.agent_db_recommendations(deployment_id, status)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_cost_samples (
		sample_id bigserial PRIMARY KEY,
		deployment_id text NOT NULL
			REFERENCES sage.agent_db_deployments(deployment_id) ON DELETE CASCADE,
		sampled_at timestamptz NOT NULL DEFAULT now(),
		cost_usd double precision NOT NULL DEFAULT 0,
		metric text NOT NULL DEFAULT '',
		value double precision NOT NULL DEFAULT 0,
		unit text NOT NULL DEFAULT '',
		detail jsonb NOT NULL DEFAULT '{}'::jsonb
	)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_backups (
		backup_id text PRIMARY KEY,
		deployment_id text NOT NULL
			REFERENCES sage.agent_db_deployments(deployment_id) ON DELETE CASCADE,
		provider text NOT NULL DEFAULT '',
		status text NOT NULL DEFAULT 'ready',
		archive_uri text NOT NULL DEFAULT '',
		verified_at timestamptz,
		restore_verified_at timestamptz,
		created_at timestamptz NOT NULL DEFAULT now(),
		detail jsonb NOT NULL DEFAULT '{}'::jsonb
	)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_tuning_hints (
		hint_id text NOT NULL,
		deployment_id text NOT NULL
			REFERENCES sage.agent_db_deployments(deployment_id) ON DELETE CASCADE,
		kind text NOT NULL,
		title text NOT NULL,
		detail text NOT NULL,
		severity text NOT NULL DEFAULT 'advisory',
		payload jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (hint_id, deployment_id)
	)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_provision_attempts (
		attempt_id bigserial PRIMARY KEY,
		deployment_id text NOT NULL
			REFERENCES sage.agent_db_deployments(deployment_id) ON DELETE CASCADE,
		kind text NOT NULL,
		status text NOT NULL,
		runner text NOT NULL DEFAULT 'dry_run',
		command jsonb NOT NULL DEFAULT '[]'::jsonb,
		exit_code integer NOT NULL DEFAULT 0,
		stdout text NOT NULL DEFAULT '',
		stderr text NOT NULL DEFAULT '',
		detail jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at timestamptz NOT NULL DEFAULT now(),
		finished_at timestamptz
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_provision_attempts_deployment
		ON sage.agent_db_provision_attempts(deployment_id, created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_audit (
		audit_id bigserial PRIMARY KEY,
		deployment_id text NOT NULL DEFAULT '',
		event text NOT NULL,
		detail jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at timestamptz NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS sage.agent_db_deploy_requests (
		deploy_request_id text PRIMARY KEY,
		deployment_id text NOT NULL
			REFERENCES sage.agent_db_deployments(deployment_id) ON DELETE CASCADE,
		tenant_id text NOT NULL,
		agent_id text NOT NULL,
		run_id text NOT NULL DEFAULT '',
		target_database_name text NOT NULL DEFAULT '',
		target_schema_name text NOT NULL DEFAULT '',
		title text NOT NULL,
		reason text NOT NULL DEFAULT '',
		status text NOT NULL DEFAULT 'draft',
		risk_tier text NOT NULL DEFAULT 'moderate',
		migration_sql text NOT NULL DEFAULT '',
		verification_sql text NOT NULL DEFAULT '',
		rollback_sql text NOT NULL DEFAULT '',
		forward_fix_notes text NOT NULL DEFAULT '',
		gate_results jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_by text NOT NULL DEFAULT '',
		reviewed_by text NOT NULL DEFAULT '',
		review_reason text NOT NULL DEFAULT '',
		created_at timestamptz NOT NULL DEFAULT now(),
		updated_at timestamptz NOT NULL DEFAULT now(),
		reviewed_at timestamptz
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_deploy_requests_deployment
		ON sage.agent_db_deploy_requests(deployment_id, created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_db_deploy_requests_tenant_status
		ON sage.agent_db_deploy_requests(tenant_id, status)`,
}
