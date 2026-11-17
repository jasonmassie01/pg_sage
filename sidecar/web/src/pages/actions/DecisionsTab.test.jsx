import { fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { DecisionsTab } from './DecisionsTab'

// policy-07: Actions > Decisions answers "why hasn't this run?" from the
// decision ledger: one row per withheld candidate, its verdict, the human
// reason, how often the gate repeated it and, for a budget park, when the
// budget frees.

const park = {
  database: 'alpha', decision_id: 41, candidate_key: 'ck_1', verdict: 'parked',
  reason: 'rate_limit_exceeded', human_reason: 'rate limit exceeded (next frees at 18:00)',
  risk_tier: 'safe', feature: 'index', target_objects: ['public.orders'],
  repeat_count: 3, open_rows: 1, first_seen: '2026-11-10T09:00:00Z',
  last_seen_at: '2026-11-10T10:00:00Z', budget_detail: 'next frees at 18:00',
  policy_version: 4, policy_doc_sha256: 'ab'.repeat(32),
}
const blocked = {
  ...park, database: 'beta', decision_id: 42, candidate_key: 'ck_2', verdict: 'blocked',
  reason: 'outside_maintenance_window', human_reason: 'outside maintenance window',
  budget_detail: undefined, repeat_count: 1, queue_id: 9,
}

let response
let query
const requested = []
vi.mock('../../hooks/useAPI', () => ({
  useAPI: url => {
    requested.push(url)
    return { data: response, loading: false, error: null, refetch: vi.fn(), ...query }
  },
}))

beforeEach(() => {
  response = { database: 'all', decisions: [park, blocked], partial: [], truncated: false,
    limit: 100 }
  query = {}
  requested.length = 0
})

describe('DecisionsTab', () => {
  it('lists each withheld candidate with its reason, repeats and budget release', () => {
    render(<DecisionsTab database="all" />)
    const rows = screen.getAllByTestId('decision-row')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('Parked')
    expect(rows[0]).toHaveTextContent('Rate limit exceeded. It is retried when the limit frees.')
    expect(rows[0]).toHaveTextContent('3 times')
    expect(rows[0]).toHaveTextContent('public.orders')
    expect(rows[0]).toHaveTextContent('alpha')
    expect(screen.getAllByTestId('decision-budget')[0]).toHaveTextContent(
      'Budget: next frees at 18:00')
    expect(rows[1]).toHaveTextContent('Outside the maintenance window')
    expect(rows[1]).not.toHaveTextContent('Budget:')
    expect(rows[1].querySelector('a[href*="actions"]')).not.toBeNull()
  })

  // ux-05b win 6: the verdict reads as the filter names it, never the enum.
  it('labels a queue_approval verdict and links to the exact queued card', () => {
    response.decisions = [{ ...blocked, verdict: 'queue_approval', reason: 'approval_required',
      human_reason: 'approval required', queue_id: 9 }]
    render(<DecisionsTab database="all" />)
    const row = screen.getByTestId('decision-row')
    expect(row).toHaveTextContent('Waiting for approval')
    expect(row).toHaveTextContent(/needs a person's approval/i)
    expect(row).not.toHaveTextContent('queue_approval')
    expect(row).not.toHaveTextContent('approval required')
    const link = screen.getByRole('link', { name: 'Approval request #9' })
    expect(link).toHaveAttribute('href', '#/actions?tab=pending&queue=9&database=beta')
  })

  // ux-05b win 13: a finding link names the finding the Cases page focuses.
  it('links a finding decision to that finding on Cases', () => {
    response.decisions = [{ ...park, finding_id: 17 }]
    render(<DecisionsTab database="all" />)
    expect(screen.getByRole('link', { name: 'Finding #17' }))
      .toHaveAttribute('href', '#/cases?finding=17&database=alpha')
  })

  it('encodes the database and ids in the link', () => {
    response.decisions = [{ ...park, database: 'a b&c', queue_id: 5 }]
    render(<DecisionsTab database="all" />)
    expect(screen.getByRole('link', { name: 'Approval request #5' }))
      .toHaveAttribute('href', '#/actions?tab=pending&queue=5&database=a+b%26c')
  })

  it('renders an unknown verdict as text, never as markup', () => {
    response.decisions = [{ ...park, verdict: '<img src=x onerror=alert(1)>' }]
    const { container } = render(<DecisionsTab database="all" />)
    expect(screen.getByTestId('decision-row')).toHaveTextContent(
      'Unknown verdict (<img src=x onerror=alert(1)>)')
    expect(container.querySelector('img')).toBeNull()
  })

  it('has no link for a row with neither queue nor finding id', () => {
    response.decisions = [{ ...park, queue_id: undefined, finding_id: undefined }]
    render(<DecisionsTab database="all" />)
    expect(screen.getByTestId('decision-row').querySelector('a')).toBeNull()
  })

  it('says no reason was recorded for an empty reason', () => {
    response.decisions = [{ ...park, reason: '', human_reason: '' }]
    render(<DecisionsTab database="all" />)
    expect(screen.getByTestId('decision-row')).toHaveTextContent('No reason recorded.')
  })

  it('shows no raw enum for any held-back reason', () => {
    const reasons = ['budget_exceeded', 'awaiting_verification', 'emergency_stop',
      'replica_mutation', 'no_typed_contract', 'ddl_conflict', 'custodian_backoff']
    response.decisions = reasons.map((reason, i) => ({ ...park, decision_id: i, reason,
      human_reason: reason.replaceAll('_', ' '), verdict: 'blocked', budget_detail: undefined,
      risk_tier: 'read_only' }))
    render(<DecisionsTab database="alpha" />)
    const rows = screen.getAllByTestId('decision-row')
    expect(rows).toHaveLength(reasons.length)
    rows.forEach((row, i) => {
      expect(row.textContent).not.toContain(reasons[i])
      // The product's own name is the one snake_case word a label may carry.
      expect(row.textContent.replaceAll('pg_sage', 'pg-sage'))
        .not.toMatch(/\b[a-z0-9]+_[a-z0-9_]+\b/)
    })
    expect(rows[2]).toHaveTextContent(
      'Emergency stop is on. Release it in the header to resume.')
  })

  it('requests the selected database and verdict filter', () => {
    render(<DecisionsTab database="alpha" />)
    expect(requested.at(-1)).toBe('/api/v1/decisions?database=alpha')
    fireEvent.change(screen.getByTestId('decisions-verdict'), { target: { value: 'parked' } })
    expect(requested.at(-1)).toBe('/api/v1/decisions?database=alpha&verdict=parked')
  })

  it('names each database that could not be read and still lists the rest', () => {
    response.partial = [{ database: 'gamma', error: 'connection' }]
    render(<DecisionsTab database="all" />)
    expect(screen.getByTestId('decisions-partial')).toHaveTextContent('gamma')
    expect(screen.getAllByTestId('decision-row')).toHaveLength(2)
  })

  it('says nothing is withheld for an empty ledger', () => {
    response.decisions = []
    render(<DecisionsTab database="all" />)
    expect(screen.getByTestId('decisions-empty')).toHaveTextContent(/nothing is withheld/i)
    expect(screen.queryAllByTestId('decision-row')).toHaveLength(0)
  })

  it('says when the list was cut at the limit', () => {
    response.truncated = true
    render(<DecisionsTab database="all" />)
    expect(screen.getByTestId('decisions-truncated')).toHaveTextContent('100')
  })

  it('shows the error when the read fails', () => {
    response = null
    query = { error: 'insufficient permissions', errorStatus: 403 }
    render(<DecisionsTab database="all" />)
    expect(screen.getByText(/insufficient permissions/)).toBeInTheDocument()
    expect(screen.queryAllByTestId('decision-row')).toHaveLength(0)
  })
})
