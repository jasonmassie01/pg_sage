import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach } from 'vitest'
import { AgentDBsPage } from './AgentDBsPage'

const refetch = vi.fn(() => Promise.resolve())

vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(url => {
    if (!url) {
      return { data: null, loading: false, error: null, refetch }
    }
    if (url === '/api/v1/agent-dbs') {
      return {
        data: {
          deployments: [{
            deployment_id: 'adb_ui',
            tenant_id: 'tenant_ui',
            agent_id: 'agent_ui',
            status: 'active',
            provider: 'databricks_lakebase',
            provisioning_level: 'instance',
            provisioning_status: 'planned',
            lease_expires_at: '2026-05-08T12:00:00Z',
          }],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    if (url === '/api/v1/agent-dbs/size-profiles') {
      return {
        data: {
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
            {
              profile_id: 'cloudsql_instance_s',
              provider: 'gcp_cloudsql',
              provisioning_level: 'instance',
              name: 'Cloud SQL instance S',
            },
          ],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    if (url === '/api/v1/agent-dbs/providers') {
      return {
        data: {
          providers: [
            {
              provider: 'databricks_lakebase',
              label: 'Databricks Lakebase',
              found: true,
              interface: 'databricks_api_or_terraform',
              detail: 'Databricks API credentials required',
            },
          ],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    if (url === '/api/v1/agent-dbs/provider-configs') {
      return {
        data: {
          provider_configs: [
            {
              provider: 'aws_rds',
              enabled: true,
              settings: {
                allowed_regions: ['us-east-1'],
                max_ttl_seconds: 3600,
                secret_token: 'super-secret-token',
              },
            },
          ],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    if (url === '/api/v1/agent-dbs/terraform-templates') {
      return {
        data: {
          terraform_templates: [
            {
              template_id: 'tf_aws_basic',
              provider: 'aws_rds',
              status: 'draft',
              manifest: ['main.tf'],
            },
            {
              template_id: 'tf_aws_approved',
              provider: 'aws_rds',
              status: 'approved',
              manifest: ['main.tf'],
            },
          ],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    if (url === '/api/v1/agent-dbs/blueprints') {
      return {
        data: {
          blueprints: [
            {
              blueprint_id: 'bp_existing',
              name: 'Reporting agent database',
              status: 'generated',
              provider: 'aws_rds',
              terraform_template_id: 'tf_aws_basic',
              generated_file: 'agentdb/bp_existing/main.tf',
              llm_used: true,
              created_by: 'planner_ui',
              blueprint: {
                region: 'us-east-1',
                budget_usd: 42,
                extensions: ['pgvector', 'postgis'],
                storage_gb: 128,
                backup_retention_days: 7,
                multi_az: true,
                pitr: true,
                private_network: true,
                public_ip: false,
              },
              policy_findings: [
                {
                  severity: 'warn',
                  message: 'Public IP disabled; private networking required',
                },
              ],
            },
            {
              blueprint_id: 'bp_approved',
              name: 'Approved analytics database',
              status: 'approved',
              provider: 'aws_rds',
              terraform_template_id: 'tf_aws_approved',
              blueprint: {
                region: 'us-east-2',
                budget_usd: 55,
                storage_gb: 64,
                backup_retention_days: 3,
              },
              policy_findings: [],
            },
          ],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    if (url === '/api/v1/agent-dbs/requests') {
      return {
        data: {
          requests: [{
            request_id: 'req_ui',
            status: 'approved',
            policy_decision: 'allow',
            requested_isolation_type: 'schema',
          }],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    if (url.endsWith('/cost')) {
      return {
        data: { cost: { total_usd: 1.25, budget_state: 'under_budget' } },
        loading: false,
        error: null,
        refetch,
      }
    }
    if (url.endsWith('/tuning-hints')) {
      return {
        data: {
          tuning_hints: [{
            hint_id: 'vector',
            kind: 'vector',
            title: 'Shape pgvector queries',
            detail: 'Use a LIMIT',
          }],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    if (url.endsWith('/backups')) {
      return {
        data: {
          backups: [{
            backup_id: 'b1',
            provider: 'managed',
            status: 'restore_verified',
            verified_at: '2026-05-08T12:05:00Z',
          }],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    if (url.endsWith('/recommendations')) {
      return {
        data: {
          recommendations: [{
            recommendation_id: 'rec_ui',
            kind: 'query_rewrite',
            title: 'Rewrite slow query',
            status: 'active',
            action_type: 'query_rewrite',
            action_risk: 'safe',
            confidence: 0.82,
            agent_instructions: {
              expected_change: 'add LIMIT before vector ORDER BY',
            },
          }],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    if (url.endsWith('/audit')) {
      return {
        data: {
          audit_events: [
            {
              audit_id: 1,
              deployment_id: 'adb_ui',
              event: 'register',
              detail: {},
              created_at: '2026-05-08T12:00:00Z',
            },
            {
              audit_id: 2,
              deployment_id: 'adb_ui',
              event: 'recommendation_feedback',
              detail: { recommendation_id: 'rec_ui', decision: 'accepted' },
              created_at: '2026-05-08T12:06:00Z',
            },
          ],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    if (url.endsWith('/deploy-requests')) {
      return {
        data: {
          deploy_requests: [{
            deploy_request_id: 'dr_ui',
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
          }],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    if (url.endsWith('/provision/attempts')) {
      return {
        data: {
          attempts: [
            {
              attempt_id: 'preflight_ui',
              kind: 'preflight',
              status: 'passed',
              created_at: '2026-05-08T12:01:00Z',
              stdout: 'Databricks API credentials validated',
            },
            {
              attempt_id: 'execute_ui',
              kind: 'execute',
              status: 'succeeded',
              created_at: '2026-05-08T12:02:00Z',
              stdout: 'dry run: cloud_api databricks_lakebase create_branch agentdb-demo',
            },
          ],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    return { data: {}, loading: false, error: null, refetch }
  }),
}))

function openTab(name) {
  fireEvent.click(screen.getByRole('tab', { name }))
}

describe('AgentDBsPage', () => {
  beforeEach(() => {
    refetch.mockClear()
    globalThis.fetch = vi.fn()
    globalThis.confirm = vi.fn(() => true)
  })

  it('renders deployments, request queue, cost, backups, and tuning hints', async () => {
    render(<AgentDBsPage />)

    expect(screen.getByTestId('agent-dbs-page')).toBeInTheDocument()
    expect(screen.getByText(/Track every agent-created database/))
      .toBeInTheDocument()
    expect(screen.getAllByText('adb_ui').length).toBeGreaterThan(0)
    openTab('Activity')
    expect(screen.getByText(/Review provision requests and policy decisions/))
      .toBeInTheDocument()
    expect(screen.getByText('req_ui')).toBeInTheDocument()
    openTab('Deployments')
    expect(await screen.findByText('Shape pgvector queries'))
      .toBeInTheDocument()
    expect(screen.getByText('$1.25')).toBeInTheDocument()
    expect(screen.getByText('under_budget')).toBeInTheDocument()
    expect(screen.getByText('Rewrite slow query')).toBeInTheDocument()
    expect(screen.getByText('Action: query_rewrite')).toBeInTheDocument()
    expect(screen.getByText('Risk: safe')).toBeInTheDocument()
    expect(screen.getByText('Confidence: 82%')).toBeInTheDocument()
    expect(screen.getByText('Instruction: add LIMIT before vector ORDER BY'))
      .toBeInTheDocument()
    expect(screen.getByTestId('agent-db-audit')).toHaveTextContent('register')
    expect(screen.getByTestId('agent-db-audit'))
      .toHaveTextContent('recommendation_feedback')
    expect(screen.getByTestId('agent-db-promotion'))
      .toHaveTextContent('Promote agent schema')
    expect(screen.getByTestId('agent-db-promotion')).toHaveTextContent(
      'review_requested',
    )
    expect(screen.getByTestId('agent-db-promotion'))
      .toHaveTextContent('review_only: true')
    openTab('Profiles')
    expect(screen.getByText(/Create reusable t-shirt sizes/))
      .toBeInTheDocument()
    expect(screen.getByText('Databricks Lakebase')).toBeInTheDocument()
    expect(screen.getByText('Lakebase instance S')).toBeInTheDocument()
    openTab('Provider Settings')
    expect(screen.getByText(/Control which cloud providers may execute live/))
      .toBeInTheDocument()
    expect(screen.getByTestId('agent-db-provision-tip-provider_settings_json'))
      .toHaveAttribute('title', expect.stringContaining('JSON policy'))
    openTab('Terraform')
    expect(screen.getByText(/Upload and review policy-checked Terraform/))
      .toBeInTheDocument()
    expect(screen.getByTestId('agent-db-provision-tip-terraform_content'))
      .toHaveAttribute('title', expect.stringContaining('Terraform source'))
    openTab('Blueprints')
    expect(screen.getByText(/Turn an English deployment intent/))
      .toBeInTheDocument()
    expect(screen.getByTestId('agent-db-provision-tip-blueprint_intent'))
      .toHaveAttribute('title', expect.stringContaining('Plain-English'))
    openTab('Deployments')
    expect(screen.getByTestId('agent-db-provisioning')).toBeInTheDocument()
    expect(screen.getByText('Run preflight')).toBeInTheDocument()
    expect(screen.getByText('Dry-run execute')).toBeInTheDocument()
    expect(screen.getByTestId('agent-db-backup-assurance'))
      .toHaveTextContent('restore_verified')
    expect(screen.getByText(
      'dry run: cloud_api databricks_lakebase create_branch agentdb-demo',
    ))
      .toBeInTheDocument()
  })

  it('submits an approved request and provisions a deployment', async () => {
    globalThis.fetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ status: 'approved', policy_decision: 'allow' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ deployment_id: 'new_dep' }),
      })

    render(<AgentDBsPage />)

    openTab('Provision')
    fireEvent.click(screen.getByTestId('agent-db-submit'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(2))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/requests')
    expect(globalThis.fetch.mock.calls[1][0]).toBe('/api/v1/agent-dbs')
    expect(JSON.parse(globalThis.fetch.mock.calls[1][1].body).provider)
      .toBe('local_postgres')
    expect(await screen.findByText('Provisioned')).toBeInTheDocument()
  })

  it('documents provision fields and submits Lakebase branch mode', async () => {
    globalThis.fetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ status: 'approved', policy_decision: 'allow' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ deployment_id: 'lakebase_branch' }),
      })

    render(<AgentDBsPage />)

    openTab('Provision')
    fireEvent.change(screen.getAllByLabelText('Provider')[0], {
      target: { value: 'databricks_lakebase' },
    })
    fireEvent.change(screen.getByLabelText('Lakebase shape'), {
      target: { value: 'autoscaling_branch' },
    })
    fireEvent.change(screen.getByLabelText('Lakebase source instance'), {
      target: { value: 'projects/demo/branches/main' },
    })

    expect(screen.getByTestId('agent-db-provision-tip-tenant_id'))
      .toHaveAttribute('title', expect.stringContaining('tenant'))
    expect(screen.getByTestId('agent-db-provision-tip-lakebase_mode'))
      .toHaveAttribute('title', expect.stringContaining('branch'))
    expect(screen.getByTestId('agent-db-provision-tip-lakebase_source_instance'))
      .toHaveAttribute('title', expect.stringContaining('existing Lakebase'))

    fireEvent.click(screen.getByTestId('agent-db-submit'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(2))
    const body = JSON.parse(globalThis.fetch.mock.calls[1][1].body)
    expect(body.provider).toBe('databricks_lakebase')
    expect(body.provisioning_level).toBe('instance')
    expect(body.metadata.lakebase_mode).toBe('autoscaling_branch')
    expect(body.metadata.provider_params).toMatchObject({
      source_instance: 'projects/demo/branches/main',
    })
  })

  it('creates a custom cloud instance size profile', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ profile_id: 'custom_lakebase' }),
    })

    render(<AgentDBsPage />)

    openTab('Profiles')
    fireEvent.change(screen.getByLabelText('Profile ID'), {
      target: { value: 'custom_lakebase' },
    })
    fireEvent.change(screen.getByLabelText('Name'), {
      target: { value: 'Custom Lakebase' },
    })
    fireEvent.change(screen.getAllByLabelText('Provider')[0], {
      target: { value: 'databricks_lakebase' },
    })
    fireEvent.click(screen.getByTestId('agent-db-profile-submit'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    const body = JSON.parse(globalThis.fetch.mock.calls[0][1].body)
    expect(body.provider).toBe('databricks_lakebase')
    expect(body.provisioning_level).toBe('instance')
    expect(await screen.findByText('Size profile saved')).toBeInTheDocument()
  })

  it('shows documented cloud settings and saves Cloud SQL provider params', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ profile_id: 'custom_cloudsql' }),
    })

    render(<AgentDBsPage />)

    openTab('Profiles')
    fireEvent.change(screen.getByLabelText('Profile ID'), {
      target: { value: 'custom_cloudsql' },
    })
    fireEvent.change(screen.getByLabelText('Name'), {
      target: { value: 'Custom Cloud SQL' },
    })
    fireEvent.change(screen.getAllByLabelText('Provider')[0], {
      target: { value: 'gcp_cloudsql' },
    })
    fireEvent.change(screen.getByLabelText('GCP project'), {
      target: { value: 'satty-488221' },
    })
    fireEvent.change(screen.getByLabelText('Cloud SQL region'), {
      target: { value: 'us-central1' },
    })
    fireEvent.change(screen.getByLabelText('Cloud SQL tier'), {
      target: { value: 'db-f1-micro' },
    })
    fireEvent.change(screen.getByLabelText('Cloud SQL edition'), {
      target: { value: 'ENTERPRISE' },
    })
    fireEvent.change(screen.getByLabelText('Public IPv4'), {
      target: { value: 'true' },
    })

    expect(screen.getByTestId('agent-db-tip-gcp_project'))
      .toHaveAttribute('title', expect.stringContaining('GCP project ID'))
    expect(screen.getByTestId('agent-db-tip-gcp_edition'))
      .toHaveAttribute('title', expect.stringContaining('Cloud SQL edition'))

    fireEvent.click(screen.getByTestId('agent-db-profile-submit'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    const body = JSON.parse(globalThis.fetch.mock.calls[0][1].body)
    expect(body.provider).toBe('gcp_cloudsql')
    expect(body.provisioning_level).toBe('instance')
    expect(body.provider_params).toMatchObject({
      project: 'satty-488221',
      region: 'us-central1',
      tier: 'db-f1-micro',
      edition: 'ENTERPRISE',
      ipv4_enabled: true,
    })
  })

  it('runs cloud provisioning preflight and dry-run execute actions', async () => {
    globalThis.fetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ status: 'passed', kind: 'preflight' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ status: 'succeeded', kind: 'execute' }),
      })

    render(<AgentDBsPage />)

    fireEvent.click(screen.getByText('Run preflight'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/adb_ui/provision/preflight')
    expect(await screen.findByText('Provision preflight passed'))
      .toBeInTheDocument()

    fireEvent.click(screen.getByText('Dry-run execute'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(2))
    expect(globalThis.fetch.mock.calls[1][0])
      .toBe('/api/v1/agent-dbs/adb_ui/provision/execute')
    expect(await screen.findByText('Provision execute succeeded'))
      .toBeInTheDocument()
  })

  it('runs live cloud execute and live destroy actions from the UI', async () => {
    globalThis.fetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ status: 'succeeded', kind: 'execute_live' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ status: 'succeeded', kind: 'destroy_live' }),
      })

    render(<AgentDBsPage />)

    fireEvent.click(screen.getByText('Live execute'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/adb_ui/provision/execute')
    expect(JSON.parse(globalThis.fetch.mock.calls[0][1].body))
      .toMatchObject({
        mode: 'live',
      })
    expect(await screen.findByText('Provision live execute succeeded'))
      .toBeInTheDocument()

    fireEvent.click(screen.getByText('Destroy live'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(2))
    expect(globalThis.fetch.mock.calls[1][0])
      .toBe('/api/v1/agent-dbs/adb_ui/provision/destroy-live')
    expect(await screen.findByText('Provision destroy live succeeded'))
      .toBeInTheDocument()
    expect(globalThis.confirm).toHaveBeenCalledWith(
      'Live destroy cloud resource for adb_ui?',
    )
  })

  it('runs cloud status, destroy dry-run, and reconcile lifecycle actions', async () => {
    globalThis.fetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ status: 'succeeded', kind: 'status_check' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ status: 'succeeded', kind: 'destroy_dry_run' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          archived: [{ deployment_id: 'adb_ui' }],
          destroy_dry_run: [{ deployment_id: 'adb_ui' }],
          blocked: [],
        }),
      })

    render(<AgentDBsPage />)

    fireEvent.click(screen.getByText('Check status'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/adb_ui/provision/status')
    expect(await screen.findByText('Provision status succeeded'))
      .toBeInTheDocument()

    fireEvent.click(screen.getByText('Destroy dry-run'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(2))
    expect(globalThis.fetch.mock.calls[1][0])
      .toBe('/api/v1/agent-dbs/adb_ui/provision/destroy-dry-run')
    expect(await screen.findByText('Provision destroy dry-run succeeded'))
      .toBeInTheDocument()

    fireEvent.click(screen.getByText('Reconcile abandoned'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(3))
    expect(globalThis.fetch.mock.calls[2][0])
      .toBe('/api/v1/agent-dbs/reconcile')
    expect(await screen.findByText(
      'Reconciled 1 archived, 1 destroy dry-run, 0 live destroy',
    ))
      .toBeInTheDocument()
  })

  it('shows backend delete guard messages for blocked deletes', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({
        error: 'delete blocked: archive deployment before deleting',
      }),
    })

    render(<AgentDBsPage />)

    fireEvent.click(screen.getByRole('button', { name: 'Delete' }))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/adb_ui')
    expect(globalThis.confirm).toHaveBeenCalledWith('delete adb_ui?')
    expect(globalThis.fetch.mock.calls[0][1]).toMatchObject({
      method: 'DELETE',
      credentials: 'include',
    })
    expect(await screen.findByText(
      'delete blocked: archive deployment before deleting',
    )).toBeInTheDocument()
  })

  it('runs backup check and restore drill dry-run actions', async () => {
    globalThis.fetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          backup_status: 'verified',
          attempt: { status: 'succeeded', kind: 'backup_check' },
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          status: 'planned',
          kind: 'restore_drill_dry_run',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          status: 'restore_verified',
          backup_id: 'restore_verified_ui',
        }),
      })

    render(<AgentDBsPage />)

    fireEvent.click(screen.getByText('Check backups'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/adb_ui/backups/check')
    expect(await screen.findByText('Backup check verified'))
      .toBeInTheDocument()

    fireEvent.click(screen.getByText('Plan restore drill'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(2))
    expect(globalThis.fetch.mock.calls[1][0])
      .toBe('/api/v1/agent-dbs/adb_ui/backups/restore-drill-dry-run')
    expect(await screen.findByText('Restore drill dry-run planned'))
      .toBeInTheDocument()

    fireEvent.click(screen.getByText('Mark restore verified'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(3))
    expect(globalThis.fetch.mock.calls[2][0])
      .toBe('/api/v1/agent-dbs/adb_ui/backups')
    expect(JSON.parse(globalThis.fetch.mock.calls[2][1].body))
      .toMatchObject({ status: 'restore_verified' })
    expect(await screen.findByText('Restore verification restore_verified'))
      .toBeInTheDocument()
  })

  it('creates and reviews promotion deploy requests', async () => {
    globalThis.fetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          deploy_request_id: 'dr_new',
          status: 'draft',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          deploy_request_id: 'dr_ui',
          status: 'approved',
        }),
      })

    render(<AgentDBsPage />)

    fireEvent.change(screen.getByLabelText('Promotion title'), {
      target: { value: 'Promote customer table' },
    })
    fireEvent.change(screen.getByLabelText('Migration SQL'), {
      target: { value: 'CREATE TABLE prod.customers(id bigint);' },
    })
    fireEvent.change(screen.getByLabelText('Verification SQL'), {
      target: { value: 'SELECT count(*) FROM prod.customers;' },
    })
    fireEvent.click(screen.getByTestId('agent-db-create-deploy-request'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/adb_ui/deploy-requests')
    expect(JSON.parse(globalThis.fetch.mock.calls[0][1].body).title)
      .toBe('Promote customer table')
    expect(await screen.findByText('Promotion draft recorded'))
      .toBeInTheDocument()

    fireEvent.click(screen.getByText('Approve'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(2))
    expect(globalThis.fetch.mock.calls[1][0])
      .toBe('/api/v1/agent-dbs/adb_ui/deploy-requests/dr_ui/approve')
    expect(await screen.findByText('Promotion approved')).toBeInTheDocument()
  })

  it('shows compact workspace tabs and only mounts the active panel', () => {
    render(<AgentDBsPage />)

    for (const name of [
      'Deployments',
      'Provision',
      'Profiles',
      'Provider Settings',
      'Terraform',
      'Blueprints',
      'Activity',
    ]) {
      expect(screen.getByRole('tab', { name })).toBeInTheDocument()
    }
    expect(screen.getByTestId('agent-db-detail')).toBeInTheDocument()
    expect(screen.queryByTestId('agent-db-form')).not.toBeInTheDocument()
    expect(screen.queryByTestId('agent-db-profiles')).not.toBeInTheDocument()

    openTab('Provision')
    expect(screen.getByTestId('agent-db-form')).toBeInTheDocument()
    expect(screen.queryByTestId('agent-db-detail')).not.toBeInTheDocument()
  })

  it('renders provider settings without exposing secret values', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        provider: 'aws_rds',
        enabled: true,
        settings: { allowed_regions: ['us-east-1'] },
      }),
    })

    render(<AgentDBsPage />)
    openTab('Provider Settings')

    expect(screen.getByTestId('agent-db-provider-settings'))
      .toHaveTextContent('aws_rds')
    expect(screen.getByTestId('agent-db-provider-settings'))
      .toHaveTextContent('allowed_regions')
    expect(screen.queryByText('super-secret-token')).not.toBeInTheDocument()
    expect(screen.getByLabelText('Provider settings JSON'))
      .not.toHaveTextContent('super-secret-token')

    fireEvent.change(screen.getByLabelText('Settings provider'), {
      target: { value: 'aws_rds' },
    })
    fireEvent.change(screen.getByLabelText('Provider settings JSON'), {
      target: { value: '{"allowed_regions":["us-east-1"]}' },
    })
    fireEvent.click(screen.getByTestId('agent-db-provider-settings-save'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/provider-configs/aws_rds')
  })

  it('uploads Terraform templates through the import panel', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        template_id: 'tf_new',
        provider: 'aws_rds',
        status: 'draft',
      }),
    })

    render(<AgentDBsPage />)
    openTab('Terraform')

    expect(screen.getByTestId('agent-db-terraform-template'))
      .toHaveTextContent('tf_aws_basic')
    fireEvent.change(screen.getByLabelText('Template ID'), {
      target: { value: 'tf_new' },
    })
    fireEvent.change(screen.getByLabelText('Template provider'), {
      target: { value: 'aws_rds' },
    })
    fireEvent.change(screen.getByLabelText('Terraform file name'), {
      target: { value: 'main.tf' },
    })
    fireEvent.change(screen.getByLabelText('Terraform content'), {
      target: { value: 'resource "aws_db_instance" "agent" {}' },
    })
    fireEvent.click(screen.getByTestId('agent-db-terraform-upload'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/terraform-templates')
    const body = JSON.parse(globalThis.fetch.mock.calls[0][1].body)
    expect(body).toMatchObject({
      template_id: 'tf_new',
      provider: 'aws_rds',
      files: [{
        path: 'main.tf',
        body: 'resource "aws_db_instance" "agent" {}',
      }],
    })
    expect(await screen.findByText('Terraform template uploaded'))
      .toBeInTheDocument()
  })

  it('approves and provisions Terraform templates from the review panel', async () => {
    globalThis.fetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ template_id: 'tf_aws_basic', status: 'approved' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ deployment_id: 'tf_aws_approved_deployment' }),
      })

    render(<AgentDBsPage />)
    openTab('Terraform')

    fireEvent.click(screen.getByTestId('agent-db-terraform-approve'))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/terraform-templates/tf_aws_basic/approve')
    expect(await screen.findByText('Terraform template approved'))
      .toBeInTheDocument()

    fireEvent.click(screen.getByTestId('agent-db-terraform-provision'))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(2))
    expect(globalThis.fetch.mock.calls[1][0])
      .toBe('/api/v1/agent-dbs/terraform-templates/tf_aws_approved/provision')
    expect(JSON.parse(globalThis.fetch.mock.calls[1][1].body))
      .toEqual(expect.objectContaining({
        deployment_id: expect.stringMatching(/^tf_aws_approved_[a-z0-9_]+_deployment$/),
        provider: 'aws_rds',
        provisioning_level: 'instance',
      }))
    expect(await screen.findByText('Terraform template provisioned as deployment'))
      .toBeInTheDocument()
  })

  it('generates blueprints and renders existing blueprint details', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        blueprint_id: 'bp_new',
        status: 'generated',
      }),
    })

    render(<AgentDBsPage />)
    openTab('Blueprints')

    expect(screen.getByTestId('agent-db-blueprints'))
      .toHaveTextContent('Reporting agent database')
    expect(screen.getByTestId('agent-db-blueprints')).toHaveTextContent(
      'generated',
    )
    expect(screen.getByTestId('agent-db-blueprints')).toHaveTextContent(
      'aws_rds',
    )
    expect(screen.getByTestId('agent-db-blueprints')).toHaveTextContent(
      'tf_aws_basic',
    )
    expect(screen.getByTestId('agent-db-blueprints')).toHaveTextContent(
      'us-east-1',
    )
    expect(screen.getByTestId('agent-db-blueprints')).toHaveTextContent('$42')
    expect(screen.getByTestId('agent-db-blueprints')).toHaveTextContent(
      'pgvector, postgis',
    )
    expect(screen.getByTestId('agent-db-blueprints')).toHaveTextContent(
      'Public IP disabled; private networking required',
    )
    expect(screen.getByTestId('agent-db-blueprints')).toHaveTextContent(
      'agentdb/bp_existing/main.tf',
    )

    fireEvent.change(screen.getByLabelText('Intent'), {
      target: { value: 'Create a private analytics database with pgvector' },
    })
    fireEvent.change(screen.getByLabelText('Blueprint ID'), {
      target: { value: 'bp_new' },
    })
    fireEvent.change(screen.getByLabelText('Blueprint name'), {
      target: { value: 'Private analytics' },
    })
    fireEvent.change(screen.getByLabelText('Blueprint provider'), {
      target: { value: 'gcp_cloudsql' },
    })
    fireEvent.change(screen.getByLabelText('Created by'), {
      target: { value: 'jmass' },
    })
    fireEvent.click(screen.getByTestId('agent-db-blueprint-submit'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/blueprints')
    expect(JSON.parse(globalThis.fetch.mock.calls[0][1].body)).toMatchObject({
      blueprint_id: 'bp_new',
      name: 'Private analytics',
      intent: 'Create a private analytics database with pgvector',
      provider: 'gcp_cloudsql',
      created_by: 'jmass',
    })
    expect(await screen.findByText('Blueprint generated')).toBeInTheDocument()
  })

  it('approves and provisions generated blueprints from the review panel', async () => {
    globalThis.fetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ blueprint_id: 'bp_existing', status: 'approved' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ deployment_id: 'bp_approved_deployment' }),
      })

    render(<AgentDBsPage />)
    openTab('Blueprints')

    fireEvent.click(screen.getByTestId('agent-db-blueprint-approve'))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/blueprints/bp_existing/approve')
    expect(await screen.findByText('Blueprint approved')).toBeInTheDocument()

    fireEvent.click(screen.getByTestId('agent-db-blueprint-provision'))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(2))
    expect(globalThis.fetch.mock.calls[1][0])
      .toBe('/api/v1/agent-dbs/blueprints/bp_approved/provision')
    expect(JSON.parse(globalThis.fetch.mock.calls[1][1].body))
      .toEqual(expect.objectContaining({
        deployment_id: expect.stringMatching(/^bp_approved_[a-z0-9_]+_deployment$/),
        tenant_id: 'tenant_agent',
        agent_id: 'agent_runner',
      }))
    expect(await screen.findByText('Blueprint provisioned as deployment'))
      .toBeInTheDocument()
  })

  it('provisions approved agent API requests from the activity panel', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ deployment_id: 'req_ui_deployment' }),
    })

    render(<AgentDBsPage />)
    openTab('Activity')

    fireEvent.click(screen.getByRole('button', { name: 'Provision' }))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/requests/req_ui/provision')
    expect(JSON.parse(globalThis.fetch.mock.calls[0][1].body))
      .toEqual(expect.objectContaining({
        deployment_id: expect.stringMatching(/^req_ui_[a-z0-9_]+_deployment$/),
      }))
    expect(await screen.findByText('Approved agent request provisioned'))
      .toBeInTheDocument()
  })
})
