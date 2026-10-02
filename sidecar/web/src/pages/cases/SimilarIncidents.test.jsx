import { render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { SimilarIncidents } from './SimilarIncidents'
import { RunbookResult } from './RunbookResult'

// Incident memory in the Cases panel (AI-SRE-SPEC §4 R2): similar past
// incidents of the same database with their verified outcomes, labeled as
// context rather than evidence; and what a signed runbook did in this
// investigation, with its proposal labeled as not executed.

let similar = { items: [] }
let requested = []
vi.mock('../../hooks/useAPI', () => ({
  useAPI: url => {
    requested.push(url)
    return { data: similar, loading: false, error: null, refetch: vi.fn() }
  },
}))

afterEach(() => {
  similar = { items: [] }
  requested = []
})

const past = (id, outcome) => ({
  label: 'similar past incident (context only, not evidence)',
  investigation_id: id, case_id: `case-${id}`, trigger_kind: 'lock_blocking',
  family: 'lock_blocking', state: 'concluded', root: 'idle_in_tx_holder',
  open: ['idle_in_tx_holder', 'ddl_lock_queue'], ruled_out: ['hot_row_contention'],
  concluded_at: '2026-09-29T10:00:00Z', score: 0.67, outcome,
})

describe('SimilarIncidents', () => {
  it('lists past incidents with their outcomes, labeled as context', () => {
    similar = { items: [
      past('a', { verdict: 'confirmed', actor: 'user:4',
        recorded_at: '2026-09-29T11:00:00Z' }),
      past('b', { verdict: 'refuted', actual_node: 'ddl_lock_queue', actor: 'user:4',
        recorded_at: '2026-09-29T11:00:00Z' }),
      past('c', null),
    ] }
    render(<SimilarIncidents database="orders" investigationId="inv-1" />)
    expect(requested).toContain('/api/v1/databases/orders/investigations/inv-1/similar')
    const box = screen.getByTestId('similar-incidents')
    expect(box).toHaveTextContent('Similar past incidents')
    expect(box).toHaveTextContent(/context only, not evidence/i)
    expect(within(box).getByTestId('similar-a')).toHaveTextContent('confirmed by an operator')
    expect(within(box).getByTestId('similar-a')).toHaveTextContent('idle_in_tx_holder')
    expect(within(box).getByTestId('similar-b'))
      .toHaveTextContent('refuted by an operator (actual: ddl_lock_queue)')
    expect(within(box).getByTestId('similar-c')).toHaveTextContent('unverified')
  })

  it('says when there are none, and tolerates an unexpected payload', () => {
    render(<SimilarIncidents database="orders" investigationId="inv-1" />)
    expect(screen.getByTestId('similar-incidents'))
      .toHaveTextContent('No similar past incidents')
    similar = { unexpected: true }
    render(<SimilarIncidents database="orders" investigationId="inv-2" />)
    expect(screen.getAllByTestId('similar-incidents')[1])
      .toHaveTextContent('No similar past incidents')
  })

  it('encodes the database and id in the URL', () => {
    render(<SimilarIncidents database="a b" investigationId="x/y" />)
    expect(requested).toContain('/api/v1/databases/a%20b/investigations/x%2Fy/similar')
  })
})

describe('RunbookResult', () => {
  const run = {
    label: 'signed runbook', runbook_id: 'rb-1', version: 3, name: 'Idle holder',
    content_hash: 'ab'.repeat(32), signed_by: 'user:1', outcome: 'completed',
    path: ['read_chains', 'is_idle', 'end_tx'], probes: 1,
    proposal: { label: 'runbook proposal (not executed)', kind: 'operator_step',
      node: 'idle_in_tx_holder', text: 'End the transaction from its application.' },
  }

  it('shows the version that ran, its path and its proposal as not executed', () => {
    render(<RunbookResult run={run} />)
    const box = screen.getByTestId('runbook-result')
    expect(box).toHaveTextContent('Idle holder')
    expect(box).toHaveTextContent('v3')
    expect(box).toHaveTextContent('user:1')
    expect(box).toHaveTextContent('read_chains → is_idle → end_tx')
    expect(box).toHaveTextContent('not executed')
    expect(box).toHaveTextContent('End the transaction from its application.')
    expect(within(box).queryByRole('button')).toBeNull()
  })

  it('shows an abstention with its reason and no proposal', () => {
    render(<RunbookResult run={{ ...run, outcome: 'abstained', proposal: null,
      reason: 'is_idle: lock_chains error (statement_timeout)' }} />)
    const box = screen.getByTestId('runbook-result')
    expect(box).toHaveTextContent('abstained')
    expect(box).toHaveTextContent('statement_timeout')
    expect(box).not.toHaveTextContent('not executed')
  })

  it('renders nothing without a run', () => {
    const { container } = render(<RunbookResult run={null} />)
    expect(container).toBeEmptyDOMElement()
  })
})
