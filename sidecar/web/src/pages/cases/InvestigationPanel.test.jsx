import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { InvestigationPanel } from './InvestigationPanel'

// Sage SRE Cases panel (AI-SRE-SPEC §9, CHECK-29): the investigation of a
// case shows its likely explanation with confidence and evidence links,
// the other and ruled-out explanations with their reasons, missing and
// failed evidence, and offers operators only the supported workflows
// (pin, export). Inconclusive is visibly not "resolved".

const EV1 = '11111111-1111-4111-8111-111111111111'
const EV2 = '22222222-2222-4222-8222-222222222222'

const concluded = {
  database: 'orders',
  investigation: {
    id: 'inv-1', state: 'concluded', trigger_kind: 'lock_blocking',
    subject: 'incident 1', pinned: false, created_at: '2026-09-27T10:00:00Z',
    summary: {
      family: 'lock_blocking', conclusive: true, root: 'idle_in_tx_holder',
      subject: 'pid 4242',
      missing: [{ probe_id: 'long_transactions', status: 'no_privilege',
        reason: 'insufficient_privilege' }],
    },
  },
  hypotheses: [
    { node: 'idle_in_tx_holder', label: 'idle-in-transaction holder',
      status: 'root_cause', confidence: 0.85, subject: 'pid 4242',
      refutation_probe: 'lock_graph',
      operator_step: 'End the transaction. pg_cancel_backend does not end an idle transaction.',
      support: [{ evidence_id: EV1, text: 'root blocker pid 4242 is idle in transaction' }],
      contradict: [] },
    { node: 'ddl_lock_queue', label: 'DDL queued behind a long transaction',
      status: 'contributing', confidence: 0.7, subject: 'pid 4242',
      refutation_probe: 'lock_graph', operator_step: 'Reschedule the DDL.',
      support: [{ evidence_id: EV1, text: 'pid 20 waits for AccessExclusiveLock' }],
      contradict: [] },
    { node: 'sage_own_action', label: "pg_sage's own change", status: 'unproven',
      confidence: 0.1, subject: 'pg_sage', refutation_probe: 'sage_actions',
      operator_step: 'Review it.',
      support: [{ evidence_id: EV2, text: 'pg_sage ran vacuum (action 7, success)' }],
      contradict: [] },
    { node: 'hot_row_contention', label: 'hot-row contention', status: 'ruled_out',
      confidence: 0, subject: 'pid 4242', refutation_probe: 'lock_graph',
      operator_step: 'Batch updates.', support: [],
      contradict: [{ evidence_id: EV1, text: 'the waits are on relation locks, not rows' }] },
  ],
  revisions: 1,
  evidence: [
    { id: EV1, probe_id: 'lock_graph', capability_state: 'available',
      observed_at: '2026-09-27T10:00:01Z', hash_verified: true, sha256: 'ab',
      payload: { rows: [{ blocker_pid: 4242 }] } },
    { id: EV2, probe_id: 'sage_actions', capability_state: 'available',
      observed_at: '2026-09-27T10:00:01Z', hash_verified: true, sha256: 'cd',
      payload: { rows: [] } },
    { id: 'ev-3', probe_id: 'long_transactions', capability_state: 'permission_denied',
      reason_code: 'insufficient_privilege', observed_at: null, hash_verified: true,
      sha256: 'ef', payload: {} },
  ],
  evidence_available: true, tombstones: [], event_count: 4, chain_verified: true,
}

let detail = concluded
vi.mock('../../hooks/useAPI', () => ({
  useAPI: url => ({ data: url ? detail : null, loading: false, error: null,
    refetch: vi.fn() }),
}))
vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

afterEach(() => {
  detail = concluded
  vi.unstubAllGlobals()
})

function open(summary, user = { role: 'viewer' }) {
  render(<InvestigationPanel database="orders" investigation={summary} user={user} />)
  fireEvent.click(screen.getByTestId('investigation-toggle'))
  return screen.getByTestId('investigation-detail')
}

describe('InvestigationPanel', () => {
  it('shows the likely explanation, confidence and evidence links', () => {
    const panel = open(concluded.investigation)
    const likely = within(panel).getByTestId('investigation-likely')
    expect(likely).toHaveTextContent('idle-in-transaction holder')
    expect(likely).toHaveTextContent('score 0.85')
    expect(likely).toHaveTextContent('strong')
    expect(likely).toHaveTextContent('DDL queued behind a long transaction')
    const link = within(likely).getAllByTestId(`evidence-link-${EV1}`)[0]
    fireEvent.click(link)
    const item = within(panel).getByTestId(`evidence-${EV1}`)
    expect(item.open).toBe(true)
    expect(item).toHaveTextContent('blocker_pid')
    expect(within(panel).getByTestId('investigation-next'))
      .toHaveTextContent('pg_cancel_backend does not end an idle transaction')
  })

  it('lists other and ruled-out explanations with their reasons', () => {
    const panel = open(concluded.investigation)
    expect(within(panel).getByTestId('investigation-other'))
      .toHaveTextContent("pg_sage's own change")
    expect(within(panel).getByTestId('investigation-ruled-out'))
      .toHaveTextContent('the waits are on relation locks, not rows')
  })

  it('shows missing and failed evidence instead of hiding it', () => {
    const panel = open(concluded.investigation)
    const missing = within(panel).getByTestId('investigation-missing')
    expect(missing).toHaveTextContent('long_transactions')
    expect(missing).toHaveTextContent('no_privilege')
    expect(within(panel).getByTestId('evidence-ev-3'))
      .toHaveTextContent('permission_denied')
  })

  it('marks an inconclusive investigation as not resolved', () => {
    const inconclusive = { ...concluded.investigation, state: 'inconclusive',
      summary: { family: 'lock_blocking', conclusive: false,
        reason: 'no lock waits at probe time' } }
    detail = { ...concluded, investigation: inconclusive,
      hypotheses: concluded.hypotheses.filter(h => h.status !== 'root_cause'
        && h.status !== 'contributing') }
    render(<InvestigationPanel database="orders" investigation={inconclusive}
      user={{ role: 'viewer' }} />)
    const badge = screen.getByTestId('investigation-state')
    expect(badge).toHaveTextContent('Inconclusive')
    expect(badge).not.toHaveTextContent('Resolved')
    expect(badge.dataset.tone).toBe('inconclusive')
    fireEvent.click(screen.getByTestId('investigation-toggle'))
    expect(screen.getByTestId('investigation-likely'))
      .toHaveTextContent('no lock waits at probe time')
  })

  it('warns when retention deleted the evidence', () => {
    detail = { ...concluded, evidence: [], evidence_available: false,
      tombstones: [{ kind: 'evidence', reason: 'retention',
        deleted_at: '2026-09-27T12:00:00Z', row_count: 3 }] }
    const panel = open(concluded.investigation)
    expect(within(panel).getByTestId('investigation-purged'))
      .toHaveTextContent('deleted by retention')
  })

  it('gives viewers no workflow controls', () => {
    const panel = open(concluded.investigation)
    expect(within(panel).queryByTestId('investigation-pin')).toBeNull()
    expect(within(panel).queryByTestId('investigation-export-json')).toBeNull()
    expect(within(panel).queryByRole('button', { name: /execute|approve|cancel/i }))
      .toBeNull()
  })

  it('lets operators pin and export, and nothing else', async () => {
    const fetch = vi.fn().mockResolvedValue({ ok: true, status: 200,
      json: async () => ({ pinned: true }) })
    vi.stubGlobal('fetch', fetch)
    const panel = open(concluded.investigation, { role: 'operator' })
    expect(within(panel).getByTestId('investigation-export-json'))
      .toHaveAttribute('href',
        '/api/v1/databases/orders/investigations/inv-1/export')
    expect(within(panel).getByTestId('investigation-export-md'))
      .toHaveAttribute('href',
        '/api/v1/databases/orders/investigations/inv-1/export?format=markdown')
    fireEvent.click(within(panel).getByTestId('investigation-pin'))
    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      '/api/v1/databases/orders/investigations/inv-1/pin',
      expect.objectContaining({ method: 'POST' })))
    const buttons = within(panel).getAllByRole('button').map(b => b.textContent)
    expect(buttons.filter(b => /execute|approve|terminate/i.test(b))).toEqual([])
  })
})
