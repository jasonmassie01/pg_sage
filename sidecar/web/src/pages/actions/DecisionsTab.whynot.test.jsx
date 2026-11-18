import { fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { DecisionsTab } from './DecisionsTab'

// ux-08b: a "Held back" row's reason line is the why-not link its gate
// reason fails (why_not from GET /api/v1/decisions), the verdict stays the
// short label, and the row opens the Why not panel for its own item.
// No concurrency test: one render tree, one read per open.

const apiGet = vi.fn()
vi.mock('../../lib/api', () => ({ apiGet: (...args) => apiGet(...args) }))
let response
vi.mock('../../hooks/useAPI', () => ({
  useAPI: () => ({ data: response, loading: false, error: null, refetch: vi.fn() }),
}))

const base = {
  database: 'alpha', decision_id: 41, candidate_key: 'ck_1', verdict: 'parked',
  reason: 'rate_limit_exceeded', human_reason: 'rate limit exceeded', risk_tier: 'safe',
  feature: 'index', target_objects: ['public.orders'], repeat_count: 3, open_rows: 1,
  last_seen_at: '2026-11-10T10:00:00Z', finding_id: 17,
  why_not: { link: 'budget', sentence: 'The change budget for this window is spent.' },
}
const byObject = { ...base, decision_id: 42, finding_id: undefined, database: 'beta',
  verdict: 'blocked', reason: 'outside_maintenance_window',
  why_not: { link: 'window', sentence: 'Outside the maintenance window.' } }
const older = { ...base, decision_id: 43, finding_id: undefined, target_objects: [],
  reason: 'live DDL preflight has warnings', why_not: undefined }

const chain = { database: 'alpha', failing: ['budget'], first_fail: 'budget',
  links: ['observed', 'finding', 'candidate', 'policy', 'window', 'budget', 'privilege',
    'trust', 'verification_wait', 'emergency_stop'].map(name => ({ name,
    status: name === 'budget' ? 'fail' : 'pass', sentence: `${name}.`,
    first_fail: name === 'budget' })) }

beforeEach(() => {
  apiGet.mockReset()
  apiGet.mockResolvedValue(chain)
  response = { database: 'all', decisions: [base, byObject, older], partial: [],
    truncated: false, limit: 100 }
})

describe('Held back rows and the Why not panel', () => {
  it('words the reason as its why-not link and keeps the verdict label', () => {
    render(<DecisionsTab database="all" />)
    const rows = screen.getAllByTestId('decision-row')
    expect(within(rows[0]).getByTestId('decision-reason')).toHaveTextContent(
      /^Change budget: The change budget for this window is spent\.$/)
    expect(rows[0]).toHaveTextContent('Parked')
    expect(within(rows[1]).getByTestId('decision-reason')).toHaveTextContent(
      /^Maintenance window: Outside the maintenance window\.$/)
    expect(within(rows[2]).getByTestId('decision-reason')).toHaveTextContent(
      'live DDL preflight has warnings')
    expect(apiGet).not.toHaveBeenCalled()
  })

  it('opens the panel for the row finding, or its object', async () => {
    render(<DecisionsTab database="all" />)
    const rows = screen.getAllByTestId('decision-row')
    fireEvent.click(within(rows[0]).getByRole('button', { name: 'Why not?' }))
    expect(await within(rows[0]).findAllByTestId('why-not-link')).toHaveLength(10)
    expect(apiGet.mock.calls[0][0]).toBe('/api/v1/whynot?database=alpha&finding_id=17')
    fireEvent.click(within(rows[1]).getByRole('button', { name: 'Why not?' }))
    await within(rows[1]).findAllByTestId('why-not-link')
    expect(apiGet.mock.calls[1][0]).toBe('/api/v1/whynot?database=beta&object=public.orders')
    fireEvent.click(within(rows[2]).getByRole('button', { name: 'Why not?' }))
    await within(rows[2]).findAllByTestId('why-not-link')
    expect(apiGet.mock.calls[2][0]).toBe('/api/v1/whynot?database=alpha')
  })
})
