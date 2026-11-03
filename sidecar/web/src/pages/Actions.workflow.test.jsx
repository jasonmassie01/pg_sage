import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

const toast = { success: vi.fn(), error: vi.fn() }
vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ range: { label: 'Last 24h' } }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({ useToast: () => toast }))

let responses
vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: path => ({ data: responses?.[path?.split('?')[0]] || {},
    loading: false, error: null, refetch: vi.fn() }),
}))

const issue = (db = 'prod', id = 'case-one') => ({
  id, database_name: db, source_type: 'finding', source_ids: ['5'],
  title: 'A query used temporary disk space', why: 'Temporary blocks were observed; user impact is unknown.',
  action_candidates: [{ action_type: 'create_index', risk_tier: 'high_risk',
    proposed_sql: 'CREATE INDEX idx_orders ON orders(customer_id)',
    blocked_reason: 'provider_unsupported', rollback_class: 'forward_fix_only',
    recovery_detail: 'Original index definition has not been captured.',
    verification_plan: ['Compare query timings with the same workload.'] }],
})
const request = { id: 10, database_name: 'prod', finding_id: 5, eligible: true,
  action_type: 'create_index', action_risk: 'high_risk', status: 'pending',
  proposed_sql: 'CREATE INDEX idx_orders ON orders(customer_id)', recommendation_id: 7 }
const card = { queue_id: 10, database: 'prod', finding: { id: 5 }, status: 'pending',
  title: 'Add an index', targets: [], sql: request.proposed_sql, risk: { tier: 'high_risk' },
  rollback: {}, why_approval: [], evidence: [] }

beforeEach(() => {
  window.history.replaceState(null, '', '/#/actions')
  responses = {
    '/api/v1/cases': { cases: [issue()] },
    '/api/v1/recommendations': { recommendations: [{ id: 7, database_name: 'prod',
      finding_id: 5, state: 'proposed', revision: 1, action_type: 'create_index', target: 'orders' }] },
    '/api/v1/actions': { actions: [], total: 0 },
    '/api/v1/actions/pending': { pending: [request], total: 1 },
  }
  toast.success.mockClear()
  toast.error.mockClear()
})
afterEach(() => vi.unstubAllGlobals())

const fleet = mode => ({ databases: [{ name: 'prod', status: { trust_level: mode } }] })

describe('Actions task workflow', () => {
  it('starts with contextual proposals, exact risk and recovery limits', () => {
    render(<Actions database="prod" user={{ role: 'admin' }} fleetData={fleet('observation')} />)
    expect(screen.getByRole('tab', { name: /Proposed changes/ })).toHaveAttribute('aria-selected', 'true')
    const proposal = screen.getByTestId('proposed-change')
    expect(proposal).toHaveTextContent('Risk: High')
    expect(proposal).toHaveTextContent('user impact is unknown')
    expect(proposal).toHaveTextContent('Original index definition has not been captured.')
    expect(proposal).toHaveTextContent('Compare query timings')
    expect(within(proposal).getByRole('link', { name: /Review case/ }))
      .toHaveAttribute('href', '#/cases?case=case-one&database=prod')
    expect(screen.getByTestId('actions-observation')).toHaveTextContent('no database changes will run in this mode')
    expect(screen.getByTestId('actions-safety-details')).toHaveTextContent(
      'Completing a baseline does not enable execution in Observation mode')
  })

  it('merges same-source aliases while labeling stored and current revision context', () => {
    responses['/api/v1/cases'].cases[0].action_candidates[0].action_type = 'create_index_concurrently'
    responses['/api/v1/recommendations'].recommendations[0].risk_tier = 'safe'
    render(<Actions database="prod" user={{ role: 'viewer' }} />)
    expect(screen.getAllByTestId('proposed-change')).toHaveLength(1)
    const proposal = screen.getByTestId('proposed-change')
    expect(screen.queryByTestId('stored-proposal-identity')).toBeNull()
    expect(proposal).toHaveTextContent('Saved proposal risk: Low')
    expect(within(proposal).getByTestId('current-proposal-context')).toHaveTextContent('Risk: High')
    expect(proposal).toHaveTextContent('This guidance may differ from the saved proposal. Review the exact SQL and recovery before deciding.')
  })

  it('withholds old stored recovery SQL when the current case recovery is unknown', async () => {
    const candidate = responses['/api/v1/cases'].cases[0].action_candidates[0]
    candidate.action_type = 'create_index_concurrently'
    candidate.rollback_class = 'unknown'
    candidate.recovery_detail = 'The created index identity is not established.'
    responses['/api/v1/recommendations/7'] = { revisions: [{ revision: 1,
      forward_sql: candidate.proposed_sql, inverse_sql: 'DROP INDEX CONCURRENTLY ON;' }] }
    render(<Actions database="prod" user={{ role: 'viewer' }} />)
    fireEvent.click(screen.getByText('Technical details and history'))
    await waitFor(() => expect(screen.getByTestId('stored-recovery-withheld')).toBeVisible())
    expect(screen.getByTestId('current-proposal-context')).toHaveTextContent('The created index identity is not established.')
    expect(screen.queryByText('DROP INDEX CONCURRENTLY ON;')).toBeNull()
    expect(screen.queryByTestId('stored-recovery-sql')).toBeNull()
    expect(screen.getByTestId('stored-proposal-sql')).toHaveTextContent('Stored recommendation r1: proposed SQL')
  })

  it('withholds a saved server-setting procedure that current provider guidance has rejected', async () => {
    const candidate = responses['/api/v1/cases'].cases[0].action_candidates[0]
    candidate.action_type = 'alter_system_guc'
    candidate.proposed_sql = ''
    candidate.rollback_class = 'not_applicable'
    candidate.blocked_reason = 'Lakebase manages instance settings; no replacement procedure has been validated.'
    candidate.policy_decision = { decision: 'blocked', blocked_reason: 'provider_managed_setting' }
    responses['/api/v1/recommendations'].recommendations[0].action_type = 'other'
    responses['/api/v1/recommendations/7'] = { revisions: [{ revision: 1,
      forward_sql: "ALTER SYSTEM SET wal_level = 'logical';", inverse_sql: 'ALTER SYSTEM RESET wal_level;' }] }
    render(<Actions database="prod" user={{ role: 'viewer' }} />)
    const saved = screen.getAllByTestId('proposed-change')[0]
    fireEvent.click(within(saved).getByText('Technical details and history'))
    await waitFor(() => expect(screen.getByTestId('stored-procedure-withheld')).toBeVisible())
    expect(screen.getByTestId('stored-procedure-withheld')).toHaveTextContent('Lakebase manages instance settings')
    expect(screen.queryByTestId('stored-proposal-sql')).toBeNull()
    expect(screen.queryByTestId('stored-recovery-sql')).toBeNull()
    expect(saved).not.toHaveTextContent("ALTER SYSTEM SET wal_level")
    expect(saved).not.toHaveTextContent('ALTER SYSTEM RESET wal_level')
  })

  it('keeps missing relation and execution availability explicit', () => {
    responses['/api/v1/cases'] = { cases: [] }
    render(<Actions database="prod" user={{ role: 'viewer' }} />)
    const proposal = screen.getByTestId('proposed-change')
    expect(proposal).toHaveTextContent('Case relation unavailable')
    expect(proposal).toHaveTextContent('Saved proposal risk: Unknown')
    expect(proposal).toHaveTextContent('Execution availability has not been confirmed')
    expect(within(proposal).getByRole('link')).toHaveAttribute('href', '#/cases?database=prod')
    expect(screen.queryByRole('tab', { name: /Needs your review/ })).toBeNull()
  })

  it('filters by the linked case without borrowing a finding from another database', () => {
    window.history.replaceState(null, '', '/#/actions?case=case-one&database=prod')
    responses['/api/v1/cases'].cases.push(issue('other', 'other-case'))
    responses['/api/v1/recommendations'].recommendations.push({ id: 8, database_name: 'other',
      finding_id: 5, state: 'proposed', action_type: 'create_index' })
    render(<Actions database="prod" user={{ role: 'admin' }} />)
    expect(screen.getAllByTestId('proposed-change')).toHaveLength(1)
    expect(screen.getByTestId('actions-case-context')).toHaveTextContent('A query used temporary disk space')
    fireEvent.click(screen.getByRole('link', { name: 'Show all changes' }))
    expect(screen.queryByTestId('actions-case-context')).toBeNull()
  })

  it('separates queued approvals, active verification and completed history', () => {
    responses['/api/v1/actions'] = { total: 3, actions: [
      { id: 10, record_kind: 'queued', status: 'pending', action_type: 'create_index' },
      { id: 11, record_kind: 'executed', outcome: 'success', action_type: 'analyze', verification_status: 'monitoring' },
      { id: 12, record_kind: 'executed', outcome: 'failed', action_type: 'vacuum' },
    ] }
    render(<Actions database="prod" user={{ role: 'admin' }} />)
    expect(screen.getByRole('tab', { name: /In progress/ })).toHaveTextContent('(1)')
    expect(screen.getByRole('tab', { name: /History/ })).toHaveTextContent('(1)')
    fireEvent.click(screen.getByRole('tab', { name: /History/ }))
    expect(screen.getByTestId('executed-actions-table')).toHaveTextContent('Failed')
    expect(screen.getByTestId('executed-actions-table')).not.toHaveTextContent('Pending approval')
    fireEvent.click(screen.getByRole('tab', { name: /In progress/ }))
    expect(screen.getByTestId('executed-actions-table')).toHaveTextContent('monitoring')
    expect(screen.getByTestId('executed-actions-table')).not.toHaveTextContent('Failed')
  })

  it('shows the same inline blocker in cards and table, including Observation mode', () => {
    responses['/api/v1/approvals'] = { cards: [card] }
    responses['/api/v1/actions/pending'].pending = [{ ...request,
      eligible: false, defer_reason: 'outside maintenance window' }]
    render(<Actions database="prod" user={{ role: 'admin' }} fleetData={fleet('observation')} />)
    fireEvent.click(screen.getByRole('tab', { name: /Needs your review/ }))
    expect(screen.getByTestId('approval-approve')).toHaveTextContent('Approve and run')
    expect(screen.getByTestId('approval-approve')).toBeDisabled()
    expect(screen.getByTestId('approval-blocker')).toHaveTextContent('Observation mode')
    fireEvent.click(screen.getByTestId('pending-view-table'))
    expect(screen.getByTestId('approve-button')).toBeDisabled()
    expect(screen.getByTestId('pending-actions-table')).toHaveTextContent('Observation mode')
  })

  it('does not claim execution from a successful approval decision alone', async () => {
    const fetch = vi.fn().mockResolvedValue({ status: 200, ok: true,
      json: async () => ({ ok: true }) })
    vi.stubGlobal('fetch', fetch)
    render(<Actions database="prod" user={{ role: 'admin' }} fleetData={fleet('advisory')} />)
    fireEvent.click(screen.getByRole('tab', { name: /Needs your review/ }))
    fireEvent.click(screen.getByTestId('approve-button'))
    await waitFor(() => expect(toast.success).toHaveBeenCalled())
    expect(toast.success).toHaveBeenCalledWith(expect.stringContaining('Execution has not been confirmed'))
    expect(toast.success).not.toHaveBeenCalledWith(expect.stringContaining('approved and executed'))
  })

  it('opens the rejection reason beside the request without requiring row expansion', () => {
    render(<Actions database="prod" user={{ role: 'admin' }} />)
    fireEvent.click(screen.getByRole('tab', { name: /Needs your review/ }))
    fireEvent.click(screen.getByTestId('reject-button'))
    expect(screen.getByRole('textbox', { name: 'Reason for rejection' })).toBeVisible()
    expect(screen.getByRole('button', { name: 'Confirm Reject' })).toBeDisabled()
  })
})
