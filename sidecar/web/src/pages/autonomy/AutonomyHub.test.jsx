import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useAPI } from '../../hooks/useAPI'
import { AutonomyHub } from './AutonomyHub'

// ux-06b: one Autonomy page. In order: each class's sentence and why, the
// waiting promotions with the one Approve, the ledger's history
// (collapsed) and the shadow ledger (collapsed, moved from Settings).

vi.mock('../../hooks/useAPI', () => ({ useAPI: vi.fn() }))

const refetch = vi.fn()
const pending = { id: 'p-1', family: 'tuning', class: 'index_create', from: 'L2', to: 'L3',
  expires_at: '2026-11-19T00:00:00Z' }
const row = (cls, extra) => ({ family: 'tuning', kind: 'self_initiated', class: cls,
  level: 'L2', effective: 'L2', cap: 'L3', reversibility: 'reversible', evidence: {},
  last_change: {}, ...extra })
const trustView = {
  enforced: true, meaning: 'the ledger grants, the operator caps',
  databases: [{ database: 'orders', floor: { known: false }, shadow: [], rows: [
    row('index_create', { mode: 'asks_first', mode_because: ['earned level L2'],
      pending }),
    row('vacuum', { family: 'hygiene', mode: 'acts_then_tells',
      mode_because: ['earned level L3', 'tier3_safe is on'], level: 'L3' }),
  ] }],
}

let trustQuery
function respond(url) {
  if (String(url).startsWith('/api/v1/trust')) return trustQuery
  return { data: null, loading: false, error: null, refetch }
}

beforeEach(() => {
  trustQuery = { data: trustView, loading: false, error: null, refetch }
  useAPI.mockImplementation(respond)
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.clearAllMocks()
})

const admin = { role: 'admin' }
const urls = () => useAPI.mock.calls.map(c => String(c[0]))

describe('AutonomyHub', () => {
  it('shows each class sentence and why before the waiting promotions', () => {
    render(<AutonomyHub database="orders" user={admin} route="#/autonomy" />)
    const modes = screen.getAllByTestId('trust-mode')
    expect(modes.map(m => m.getAttribute('data-mode')))
      .toEqual(['asks_first', 'acts_then_tells'])
    expect(modes[0]).toHaveTextContent('asks first')
    expect(modes[0]).toHaveTextContent('earned level L2')
    expect(modes[1]).toHaveTextContent('acts, then tells you')
    const hub = screen.getByTestId('autonomy-hub')
    const order = ['trust-page', 'autonomy-waiting', 'autonomy-history', 'autonomy-shadow']
      .map(id => hub.innerHTML.indexOf(`data-testid="${id}"`))
    expect(order.every(i => i >= 0)).toBe(true)
    expect([...order].sort((a, b) => a - b)).toEqual(order)
  })

  it('keeps history and the shadow ledger collapsed and unread', () => {
    render(<AutonomyHub database="orders" user={admin} route="#/trust" />)
    expect(screen.getByTestId('autonomy-history')).not.toHaveAttribute('open')
    expect(screen.queryByTestId('autonomy-page')).toBeNull()
    expect(urls().some(u => u.startsWith('/api/v1/sre/autonomy'))).toBe(false)
    expect(urls().some(u => u.startsWith('/api/v1/shadow-decisions'))).toBe(false)
  })

  it('opens the history section for the old earned-autonomy path', () => {
    render(<AutonomyHub database="orders" user={admin} route="#/advanced/autonomy" />)
    expect(screen.getByTestId('autonomy-history')).toHaveAttribute('open')
    expect(urls()).toContain('/api/v1/sre/autonomy?database=orders')
    expect(screen.getByTestId('autonomy-shadow')).not.toHaveAttribute('open')
  })

  it('has one Approve per waiting promotion, and approving re-reads', async () => {
    const fetchMock = vi.fn(async () => ({ ok: true, status: 200, json: async () => ({}) }))
    vi.stubGlobal('fetch', fetchMock)
    render(<AutonomyHub database="orders" user={admin} route="#/autonomy" />)
    expect(screen.getAllByRole('button', { name: 'Approve' })).toHaveLength(1)
    const waiting = screen.getByTestId('autonomy-waiting-orders-tuning-index_create')
    fireEvent.click(within(waiting).getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(refetch).toHaveBeenCalled())
    // ModelLift and TuningCalibration read with fetch too; find the decision.
    const posts = fetchMock.mock.calls.filter(([, init]) => init?.method === 'POST')
    expect(posts.map(([url]) => url))
      .toEqual(['/api/v1/sre/autonomy/proposals/p-1/approve?database=orders'])
    expect(JSON.parse(posts[0][1].body).note).toBe('approved on the Trust page')
  })

  it('shows the conflict when a second admin approves, and re-reads', async () => {
    vi.stubGlobal('fetch', vi.fn(async (url, init) => (init?.method === 'POST'
      ? { ok: false, status: 409,
        json: async () => ({ error: 'autonomy proposal is not pending' }) }
      : { ok: true, status: 200, json: async () => ({}) })))
    render(<AutonomyHub database="orders" user={admin} route="#/autonomy" />)
    fireEvent.click(screen.getByRole('button', { name: 'Approve' }))
    expect(await screen.findByText('autonomy proposal is not pending')).toBeInTheDocument()
    expect(refetch).toHaveBeenCalled()
  })

  it('lets an operator reject but not approve, and a viewer neither', () => {
    const { unmount } = render(<AutonomyHub database="orders" user={{ role: 'operator' }} />)
    expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull()
    expect(screen.getAllByRole('button', { name: 'Reject' })).toHaveLength(1)
    unmount()
    render(<AutonomyHub database="orders" user={{ role: 'viewer' }} />)
    expect(screen.queryByRole('button', { name: /Approve|Reject/ })).toBeNull()
  })

  it('shows the read error and no sentence column when the trust read fails', () => {
    trustQuery = { data: null, loading: false, error: 'trust read failed: 503', refetch }
    render(<AutonomyHub database="orders" user={admin} />)
    expect(screen.getByText(/trust read failed: 503/)).toBeInTheDocument()
    expect(screen.queryByTestId('trust-mode')).toBeNull()
    expect(screen.queryByText('What pg_sage may do')).toBeNull()
  })

  it('says no action classes yet for a database without ledger rows', () => {
    trustQuery = { ...trustQuery, data: { ...trustView,
      databases: [{ database: 'orders', floor: { known: false }, rows: [] }] } }
    render(<AutonomyHub database="orders" user={admin} />)
    expect(screen.getByTestId('trust-no-classes-orders'))
      .toHaveTextContent('No action classes yet')
    expect(screen.getByTestId('autonomy-waiting-empty')).toHaveTextContent('Nothing waits')
  })

  it('omits the sentence column for rows an older server sent without a mode', () => {
    trustQuery = { ...trustQuery, data: { ...trustView, databases: [{ database: 'orders',
      floor: { known: false }, rows: [row('vacuum', { family: 'hygiene' })] }] } }
    render(<AutonomyHub database="orders" user={admin} />)
    expect(screen.queryByTestId('trust-mode')).toBeNull()
    expect(screen.getByTestId('trust-row-orders-hygiene-vacuum')).toBeInTheDocument()
  })
})
