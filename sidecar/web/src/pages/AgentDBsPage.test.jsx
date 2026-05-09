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
              version: 'Databricks CLI v0.298.0',
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
              stdout: 'databricks CLI detected',
            },
            {
              attempt_id: 'execute_ui',
              kind: 'execute',
              status: 'succeeded',
              created_at: '2026-05-08T12:02:00Z',
              stdout: 'dry run: databricks database branches create agentdb-demo',
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

describe('AgentDBsPage', () => {
  beforeEach(() => {
    refetch.mockClear()
    globalThis.fetch = vi.fn()
  })

  it('renders deployments, request queue, cost, backups, and tuning hints', async () => {
    render(<AgentDBsPage />)

    expect(screen.getByTestId('agent-dbs-page')).toBeInTheDocument()
    expect(screen.getAllByText('adb_ui').length).toBeGreaterThan(0)
    expect(screen.getByText('req_ui')).toBeInTheDocument()
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
    expect(screen.getByText('Databricks Lakebase')).toBeInTheDocument()
    expect(screen.getByText('Lakebase instance S')).toBeInTheDocument()
    expect(screen.getByTestId('agent-db-provisioning')).toBeInTheDocument()
    expect(screen.getByText('Run preflight')).toBeInTheDocument()
    expect(screen.getByText('Dry-run execute')).toBeInTheDocument()
    expect(screen.getByTestId('agent-db-backup-assurance'))
      .toHaveTextContent('restore_verified')
    expect(screen.getByText(
      'dry run: databricks database branches create agentdb-demo',
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

    fireEvent.click(screen.getByTestId('agent-db-submit'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(2))
    expect(globalThis.fetch.mock.calls[0][0])
      .toBe('/api/v1/agent-dbs/requests')
    expect(globalThis.fetch.mock.calls[1][0]).toBe('/api/v1/agent-dbs')
    expect(JSON.parse(globalThis.fetch.mock.calls[1][1].body).provider)
      .toBe('local_postgres')
    expect(await screen.findByText('Provisioned')).toBeInTheDocument()
  })

  it('creates a custom cloud instance size profile', async () => {
    globalThis.fetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ profile_id: 'custom_lakebase' }),
    })

    render(<AgentDBsPage />)

    fireEvent.change(screen.getByLabelText('Profile ID'), {
      target: { value: 'custom_lakebase' },
    })
    fireEvent.change(screen.getByLabelText('Name'), {
      target: { value: 'Custom Lakebase' },
    })
    fireEvent.change(screen.getAllByLabelText('Provider')[1], {
      target: { value: 'databricks_lakebase' },
    })
    fireEvent.click(screen.getByTestId('agent-db-profile-submit'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    const body = JSON.parse(globalThis.fetch.mock.calls[0][1].body)
    expect(body.provider).toBe('databricks_lakebase')
    expect(body.provisioning_level).toBe('instance')
    expect(await screen.findByText('Size profile saved')).toBeInTheDocument()
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
    expect(await screen.findByText('Reconciled 1 archived, 1 destroy dry-run'))
      .toBeInTheDocument()
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
})
