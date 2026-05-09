import { type Page } from '@playwright/test'

export const mockAgentDBs = {
  deployments: [
    {
      deployment_id: 'agentdb-demo',
      tenant_id: 'tenant_demo',
      agent_id: 'agent_researcher',
      status: 'active',
      isolation_type: 'schema',
      provider: 'databricks_lakebase',
      provisioning_level: 'instance',
      provisioning_status: 'planned',
      size_profile_id: 'lakebase_instance_s',
      schema_name: 'agentdb_demo',
      budget_usd: 25,
      lease_expires_at: '2026-05-08T12:00:00Z',
      metadata: {
        workload_types: ['vector', 'jsonb'],
        extensions: ['pgvector', 'pg_stat_statements'],
      },
    },
  ],
}

export const mockAgentDBProfiles = {
  profiles: [
    {
      profile_id: 'local_schema_xs',
      provider: 'local_postgres',
      provisioning_level: 'schema',
      name: 'Local schema XS',
    },
    {
      profile_id: 'lakebase_instance_s',
      provider: 'databricks_lakebase',
      provisioning_level: 'instance',
      name: 'Lakebase instance S',
    },
  ],
}

export const mockAgentDBProviders = {
  providers: [
    {
      provider: 'aws_rds',
      label: 'AWS RDS',
      cli: 'aws',
      found: true,
      version: 'aws-cli/2.34.19',
    },
    {
      provider: 'gcp_cloudsql',
      label: 'GCP Cloud SQL',
      cli: 'gcloud',
      found: true,
      version: 'Google Cloud SDK 557.0.0',
    },
    {
      provider: 'databricks_lakebase',
      label: 'Databricks Lakebase',
      cli: 'databricks',
      found: true,
      version: 'Databricks CLI v0.298.0',
    },
  ],
}

export const mockAgentDBRequests = {
  requests: [
    {
      request_id: 'req-agentdb-demo',
      tenant_id: 'tenant_demo',
      agent_id: 'agent_researcher',
      status: 'approved',
      policy_decision: 'allow',
      requested_isolation_type: 'schema',
    },
  ],
}

export const mockAgentDBCost = {
  cost: {
    deployment_id: 'agentdb-demo',
    total_usd: 3.45,
    budget_usd: 25,
    budget_state: 'under_budget',
    budget_action: 'none',
  },
}

export const mockAgentDBHints = {
  tuning_hints: [
    {
      hint_id: 'pack_vector_query_shape',
      kind: 'vector',
      title: 'Shape pgvector queries for bounded ANN scans',
      detail: 'Use a LIMIT with vector ORDER BY.',
      severity: 'advisory',
    },
    {
      hint_id: 'pack_jsonb_index_shapes',
      kind: 'jsonb',
      title: 'Match JSONB indexes to access patterns',
      detail: 'Use expression indexes for scalar extraction.',
      severity: 'advisory',
    },
  ],
}

export const mockAgentDBBackups = {
  backups: [
    {
      backup_id: 'backup-agentdb-demo',
      deployment_id: 'agentdb-demo',
      provider: 'managed',
      status: 'restore_verified',
      verified_at: '2026-05-08T12:05:00Z',
    },
  ],
}

export const mockAgentDBRecommendations = {
  recommendations: [
    {
      recommendation_id: 'rec-agentdb-demo',
      kind: 'query_rewrite',
      title: 'Add LIMIT to vector lookup',
      status: 'active',
      action_type: 'query_rewrite',
      action_risk: 'safe',
      confidence: 0.82,
      agent_instructions: {
        expected_change: 'add LIMIT before vector ORDER BY',
      },
    },
  ],
}

export const mockAgentDBAudit = {
  audit_events: [
    {
      audit_id: 1,
      deployment_id: 'agentdb-demo',
      event: 'register',
      detail: {},
      created_at: '2026-05-08T12:00:00Z',
    },
    {
      audit_id: 2,
      deployment_id: 'agentdb-demo',
      event: 'recommendation_feedback',
      detail: { recommendation_id: 'rec-agentdb-demo', decision: 'accepted' },
      created_at: '2026-05-08T12:06:00Z',
    },
  ],
}

export const mockAgentDBDeployRequests = {
  deploy_requests: [
    {
      deploy_request_id: 'dr-agentdb-demo',
      deployment_id: 'agentdb-demo',
      title: 'Promote agent schema',
      status: 'review_requested',
      risk_tier: 'moderate',
      migration_sql: 'CREATE TABLE prod.agent_items(id bigint primary key);',
      verification_sql: 'SELECT count(*) FROM prod.agent_items;',
      rollback_sql: 'DROP TABLE prod.agent_items;',
      gate_results: {
        review_only: true,
        schema_lint: 'pass',
      },
    },
  ],
}

export const mockAgentDBProvisionAttempts = {
  attempts: [
    {
      attempt_id: 'attempt-preflight-demo',
      deployment_id: 'agentdb-demo',
      kind: 'preflight',
      status: 'passed',
      created_at: '2026-05-08T12:01:00Z',
      stdout: 'databricks CLI detected\nworkspace reachable',
    },
    {
      attempt_id: 'attempt-execute-demo',
      deployment_id: 'agentdb-demo',
      kind: 'execute',
      status: 'succeeded',
      created_at: '2026-05-08T12:02:00Z',
      stdout: 'dry run: databricks database branches create agentdb-demo',
    },
    {
      attempt_id: 'attempt-status-demo',
      deployment_id: 'agentdb-demo',
      kind: 'status_check',
      status: 'succeeded',
      created_at: '2026-05-08T12:03:00Z',
      stdout: 'dry run: databricks database branches get agentdb-demo',
    },
    {
      attempt_id: 'attempt-destroy-demo',
      deployment_id: 'agentdb-demo',
      kind: 'destroy_dry_run',
      status: 'succeeded',
      created_at: '2026-05-08T12:04:00Z',
      stdout: 'dry run: databricks database branches delete agentdb-demo',
    },
  ],
}

export const mockAgentDBReconcile = {
  archived: [mockAgentDBs.deployments[0]],
  destroy_dry_run: [mockAgentDBProvisionAttempts.attempts[3]],
  blocked: [],
}

export const mockAgentDBBackupCheck = {
  backup_status: 'verified',
  provider_mode: 'managed_provider',
  safe_for_destroy: false,
  attempt: {
    attempt_id: 'attempt-backup-check-demo',
    deployment_id: 'agentdb-demo',
    kind: 'backup_check',
    status: 'succeeded',
  },
}

export const mockAgentDBRestoreDrill = {
  attempt_id: 'attempt-restore-drill-demo',
  deployment_id: 'agentdb-demo',
  kind: 'restore_drill_dry_run',
  status: 'planned',
}

export async function registerAgentDBAPIs(page: Page) {
  await page.route('**/api/v1/agent-dbs**', route => {
    const url = route.request().url()
    const method = route.request().method()
    if (method === 'DELETE') {
      return route.fulfill({ status: 200, json: { deleted: true } })
    }
    if (url.includes('/providers')) {
      return route.fulfill({ json: mockAgentDBProviders })
    }
    if (url.includes('/size-profiles')) {
      if (method === 'POST') {
        return route.fulfill({ status: 200, json: mockAgentDBProfiles.profiles[1] })
      }
      return route.fulfill({ json: mockAgentDBProfiles })
    }
    if (url.includes('/requests')) {
      if (method === 'POST') {
        return route.fulfill({
          status: 200,
          json: {
            request_id: 'req-ui-created',
            status: 'approved',
            policy_decision: 'allow',
          },
        })
      }
      return route.fulfill({ json: mockAgentDBRequests })
    }
    if (url.endsWith('/cost')) return route.fulfill({ json: mockAgentDBCost })
    if (url.endsWith('/tuning-hints')) return route.fulfill({ json: mockAgentDBHints })
    if (url.endsWith('/backups')) return route.fulfill({ json: mockAgentDBBackups })
    if (url.endsWith('/backups/check')) {
      return route.fulfill({ status: 200, json: mockAgentDBBackupCheck })
    }
    if (url.endsWith('/backups/restore-drill-dry-run')) {
      return route.fulfill({ status: 200, json: mockAgentDBRestoreDrill })
    }
    if (url.endsWith('/recommendations')) {
      return route.fulfill({ json: mockAgentDBRecommendations })
    }
    if (url.endsWith('/audit')) {
      return route.fulfill({ json: mockAgentDBAudit })
    }
    if (url.endsWith('/deploy-requests')) {
      if (method === 'POST') {
        return route.fulfill({
          status: 200,
          json: { deploy_request_id: 'dr-created', status: 'draft' },
        })
      }
      return route.fulfill({ json: mockAgentDBDeployRequests })
    }
    if (url.endsWith('/deploy-requests/dr-agentdb-demo/approve')) {
      return route.fulfill({
        status: 200,
        json: { ...mockAgentDBDeployRequests.deploy_requests[0], status: 'approved' },
      })
    }
    if (url.endsWith('/deploy-requests/dr-agentdb-demo/deny')) {
      return route.fulfill({
        status: 200,
        json: { ...mockAgentDBDeployRequests.deploy_requests[0], status: 'denied' },
      })
    }
    if (url.endsWith('/provision/attempts')) {
      return route.fulfill({ json: mockAgentDBProvisionAttempts })
    }
    if (url.endsWith('/provision/preflight')) {
      return route.fulfill({ status: 200, json: mockAgentDBProvisionAttempts.attempts[0] })
    }
    if (url.endsWith('/provision/execute')) {
      return route.fulfill({ status: 200, json: mockAgentDBProvisionAttempts.attempts[1] })
    }
    if (url.endsWith('/provision/status')) {
      return route.fulfill({ status: 200, json: mockAgentDBProvisionAttempts.attempts[2] })
    }
    if (url.endsWith('/provision/destroy-dry-run')) {
      return route.fulfill({ status: 200, json: mockAgentDBProvisionAttempts.attempts[3] })
    }
    if (method === 'POST' && url.endsWith('/api/v1/agent-dbs')) {
      return route.fulfill({ status: 200, json: mockAgentDBs.deployments[0] })
    }
    if (method === 'POST' && url.endsWith('/api/v1/agent-dbs/cleanup')) {
      return route.fulfill({ status: 200, json: { archived: mockAgentDBs.deployments } })
    }
    if (method === 'POST' && url.endsWith('/api/v1/agent-dbs/reconcile')) {
      return route.fulfill({ status: 200, json: mockAgentDBReconcile })
    }
    if (method === 'POST') {
      return route.fulfill({ status: 200, json: mockAgentDBs.deployments[0] })
    }
    return route.fulfill({ json: mockAgentDBs })
  })
}
