import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ActionProposals } from './ActionProposal'

// Sage SRE M5 in the Cases panel (AI-SRE-SPEC §9): the proposed action
// with its exact target, evidence, repair contract and policy verdict;
// approve/deny through the existing approval API; the recovery result.
// Viewers see everything and can do nothing. Nothing executes from here
// except an operator's approval of an already requested item.

const EV = '11111111-1111-4111-8111-111111111111'

const requested = {
  id: 'p-1', state: 'requested', class: 'cancel_backend', family: 'lock_blocking',
  node: 'ddl_lock_queue', sql: 'SELECT pg_cancel_backend(5151)',
  evidence_ids: [EV],
  target: { pid: 5151, user: 'app', database: 'orders', backend_start:
    '2026-10-01T09:00:00Z', query_start: '2026-10-01T09:01:00Z',
    query_hash: 'c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00',
    blocking: 2, observed_at: '2026-10-01T09:01:05Z' },
  contract: { action_type: 'cancel_backend', reversibility: 'mitigation_only',
    scope: 'one backend, derived from the evidence ids',
    preconditions: ['identity evidence at most 5 s old'],
    post_conditions: ['the target wait edges clear'],
    never_do: ['never pg_terminate_backend'],
    blast_radius: { max_backends: 1, max_databases: 1, max_actions_per_investigation: 1 },
    residual_risk: 'a pid-based signal can race with the query finishing' },
  policy: { decision: 'execute', risk_tier: 'moderate' },
  approval: { queue_id: 42, status: 'pending', expires_at: '2026-10-01T09:16:00Z' },
  recovery: { state: '' },
}

let proposals = [requested]
vi.mock('../../hooks/useAPI', () => ({
  useAPI: url => ({ data: url ? { items: proposals } : null, loading: false,
    error: null, refetch: vi.fn() }),
}))
vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

afterEach(() => {
  proposals = [requested]
  vi.unstubAllGlobals()
})

function show(user = { role: 'operator' }) {
  render(<ActionProposals database="orders" investigationId="inv-1" user={user} />)
  return screen.getByTestId('action-proposal-p-1')
}

describe('ActionProposals', () => {
  it('shows the exact target, contract, policy verdict and residual risk', () => {
    const card = show({ role: 'viewer' })
    expect(card).toHaveTextContent('pg_cancel_backend(5151)')
    expect(card).toHaveTextContent('pid 5151')
    expect(card).toHaveTextContent('user app')
    expect(card).toHaveTextContent('mitigation_only')
    expect(card).toHaveTextContent('never pg_terminate_backend')
    expect(card).toHaveTextContent('race with the query finishing')
    expect(card).toHaveTextContent('Awaiting approval')
    expect(within(card).getByTestId(`evidence-link-${EV}`))
      .toHaveAttribute('href', `#evidence-${EV}`)
  })

  it('gives viewers no controls', () => {
    const card = show({ role: 'viewer' })
    expect(within(card).queryAllByRole('button')).toEqual([])
  })

  it('approves through the existing approval API', async () => {
    const fetch = vi.fn().mockResolvedValue({ ok: true, status: 200,
      json: async () => ({ executed: true }) })
    vi.stubGlobal('fetch', fetch)
    const card = show()
    fireEvent.click(within(card).getByTestId('action-approve'))
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      '/api/v1/actions/42/approve?database=orders',
      expect.objectContaining({ method: 'POST' })))
  })

  it('denies with a reason only', async () => {
    const fetch = vi.fn().mockResolvedValue({ ok: true, status: 200,
      json: async () => ({ status: 'rejected' }) })
    vi.stubGlobal('fetch', fetch)
    const card = show()
    const deny = within(card).getByTestId('action-deny')
    expect(deny).toBeDisabled()
    fireEvent.change(within(card).getByTestId('action-deny-reason'),
      { target: { value: 'blocker is the nightly export' } })
    fireEvent.click(deny)
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      '/api/v1/actions/42/reject?database=orders',
      expect.objectContaining({ method: 'POST',
        body: JSON.stringify({ reason: 'blocker is the nightly export' }) })))
  })

  it('requests approval for a proposal the policy allows', async () => {
    proposals = [{ ...requested, state: 'proposed', approval: null }]
    const fetch = vi.fn().mockResolvedValue({ ok: true, status: 200,
      json: async () => ({ state: 'requested' }) })
    vi.stubGlobal('fetch', fetch)
    const card = show()
    expect(within(card).queryByTestId('action-approve')).toBeNull()
    fireEvent.click(within(card).getByTestId('action-request'))
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      '/api/v1/databases/orders/investigations/inv-1/proposals/p-1/request',
      expect.objectContaining({ method: 'POST' })))
  })

  it('explains a withheld policy instead of offering approval', () => {
    proposals = [{ ...requested, state: 'proposed', approval: null,
      policy: { decision: 'blocked', blocked_reason: 'observe_only' } }]
    const card = show()
    expect(card).toHaveTextContent('observe_only')
    expect(within(card).queryByTestId('action-request')).toBeNull()
  })

  it('shows why nothing was proposed', () => {
    proposals = [{ id: 'p-1', state: 'ineligible', reason: 'idle_in_transaction',
      detail: 'pg_cancel_backend does not end an idle transaction', class: 'cancel_backend' }]
    const card = show()
    expect(card).toHaveTextContent('Not proposed')
    expect(card).toHaveTextContent('pg_cancel_backend does not end an idle transaction')
    expect(within(card).queryAllByRole('button')).toEqual([])
  })

  it('shows the recovery verdict after execution', () => {
    proposals = [{ ...requested, state: 'executed',
      approval: { queue_id: 42, status: 'executed' },
      recovery: { state: 'recovered', attribution: 'pg_sage',
        verdict: 'wait edges cleared, lock waits fell from 2 to 0',
        samples: [{}, {}, {}] } }]
    const card = show()
    expect(within(card).getByTestId('action-recovery')).toHaveTextContent('recovered')
    expect(within(card).getByTestId('action-recovery'))
      .toHaveTextContent('lock waits fell from 2 to 0')
    expect(within(card).getByTestId('action-recovery')).toHaveTextContent('3 samples')
    expect(within(card).queryByTestId('action-approve')).toBeNull()
  })

  it('renders nothing without proposals', () => {
    proposals = []
    render(<ActionProposals database="orders" investigationId="inv-1"
      user={{ role: 'operator' }} />)
    expect(screen.queryByTestId('action-proposals')).toBeNull()
  })
})
