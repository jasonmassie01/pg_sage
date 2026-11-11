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
    expect(rows[0]).toHaveTextContent('parked')
    expect(rows[0]).toHaveTextContent('rate limit exceeded (next frees at 18:00)')
    expect(rows[0]).toHaveTextContent('3 times')
    expect(rows[0]).toHaveTextContent('public.orders')
    expect(rows[0]).toHaveTextContent('alpha')
    expect(screen.getAllByTestId('decision-budget')[0]).toHaveTextContent(
      'Budget: next frees at 18:00')
    expect(rows[1]).toHaveTextContent('outside maintenance window')
    expect(rows[1]).not.toHaveTextContent('Budget:')
    expect(rows[1].querySelector('a[href*="actions"]')).not.toBeNull()
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
