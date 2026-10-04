import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApprovalCard, approvalOutcomeMessage } from './ApprovalCard'

const toast = { success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() }
vi.mock('../../components/Toast', () => ({ useToast: () => toast }))

function card(overrides = {}) {
  return {
    queue_id: 41,
    database: 'orders',
    title: 'Index recommendation for public.orders',
    action_type: 'create_index_concurrently',
    status: 'pending',
    targets: ['public.orders'],
    finding: { id: 12, title: 'Index recommendation for public.orders' },
    evidence: [
      { kind: 'finding', label: 'Finding #12', value: 'warning', ref: 'finding:12' },
      { kind: 'metric', label: 'seq scan', value: '18233', ref: 'finding:12' },
      { kind: 'query', label: 'Query 4242', value: 'SELECT * FROM orders WHERE customer_id = $1',
        ref: 'queryid:4242' },
    ],
    rationale: { source: 'llm', text: 'Seq scans on orders filter by customer_id',
      confidence: 0.82 },
    predicted_effect: { improvement_pct: 38.5, estimated_size_bytes: 12582912,
      affected_queries: ['SELECT * FROM orders WHERE customer_id = $1'],
      what_if_verdict: 'unverified', method: 'llm_estimate' },
    sql: 'CREATE INDEX CONCURRENTLY idx_orders_customer ON public.orders (customer_id)',
    rollback: { class: 'reversible', sql: 'DROP INDEX CONCURRENTLY public.idx_orders_customer',
      note: '' },
    risk: { tier: 'moderate', blast_radius: '1 index on public.orders',
      lock: 'SHARE UPDATE EXCLUSIVE on the table', guardrails: ['maintenance_window'],
      post_checks: ['index is valid'] },
    why_approval: [
      { code: 'what_if_unverified', text: 'HypoPG has not verified the improvement' },
      { code: 'trust_level', text: 'Trust level is advisory' },
    ],
    proposed_at: '2026-10-03T11:00:00Z',
    expires_at: new Date(Date.now() + 5 * 3600 * 1000).toISOString(),
    card_hash: 'hash-41',
    ...overrides,
  }
}

function mockFetch(status, body) {
  globalThis.fetch = vi.fn().mockResolvedValue({
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  })
}

beforeEach(() => {
  Object.values(toast).forEach(fn => fn.mockClear())
})

afterEach(() => {
  delete globalThis.fetch
})

describe('ApprovalCard', () => {
  it('shows the what, the why and the evidence', () => {
    render(<ApprovalCard card={card()} />)
    expect(screen.getByTestId('approval-card-title'))
      .toHaveTextContent('Index recommendation for public.orders')
    const why = screen.getByTestId('approval-why')
    expect(why).toHaveTextContent('Why it needs you')
    expect(why).toHaveTextContent('HypoPG has not verified the improvement')
    expect(why).toHaveTextContent('Trust level is advisory')
    const evidence = screen.getByTestId('approval-evidence')
    expect(evidence).toHaveTextContent('seq scan')
    expect(evidence).toHaveTextContent('18233')
    expect(evidence).toHaveTextContent('queryid:4242')
    expect(screen.getByTestId('approval-rationale'))
      .toHaveTextContent('Seq scans on orders filter by customer_id')
    expect(screen.getByTestId('approval-rationale')).toHaveTextContent('82%')
    const predicted = screen.getByTestId('approval-predicted')
    expect(predicted).toHaveTextContent('38.5%')
    expect(predicted).toHaveTextContent('12.0 MB')
    expect(predicted).toHaveTextContent('unverified')
    expect(screen.getByTestId('approval-sql'))
      .toHaveTextContent('CREATE INDEX CONCURRENTLY idx_orders_customer')
    expect(screen.getByTestId('approval-rollback'))
      .toHaveTextContent('DROP INDEX CONCURRENTLY public.idx_orders_customer')
    const risk = screen.getByTestId('approval-risk')
    expect(risk).toHaveTextContent('moderate')
    expect(risk).toHaveTextContent('1 index on public.orders')
    expect(risk).toHaveTextContent('maintenance_window')
    expect(screen.getByTestId('approval-expiry')).toHaveTextContent(/Expires in [45]h/)
  })

  it('renders a minimal card without optional sections', () => {
    render(<ApprovalCard card={card({ rationale: null, evidence: [],
      predicted_effect: {}, rollback: { class: '', sql: '', note: 'Nothing to undo' },
      why_approval: [], targets: [] })} />)
    expect(screen.queryByTestId('approval-rationale')).toBeNull()
    expect(screen.queryByTestId('approval-predicted')).toBeNull()
    expect(screen.getByTestId('approval-why'))
      .toHaveTextContent('The policy queued this change for approval')
    expect(screen.getByTestId('approval-rollback')).toHaveTextContent('Nothing to undo')
  })

  it('shows the class trust as one line with a link to the Trust page', () => {
    const line = 'Trust: tuning/index_create at L1 (cap L3); evidence 1 improved, ' +
      '0 neutral, 0 regressed, 0 rolled back, 0 rejected; next L2: 2 more successes'
    render(<ApprovalCard card={card({ trust: { family: 'tuning', class: 'index_create',
      level: 'L1', cap: 'L3', next_level: 'L2', line,
      evidence: { improved: 1, neutral: 0, regressed: 0, rolled_back: 0, rejected: 0 } } })} />)
    const trust = screen.getByTestId('approval-trust')
    expect(trust).toHaveTextContent(line)
    expect(within(trust).getByRole('link')).toHaveAttribute('href', '#/trust')
  })

  it('shows unavailable trust and nothing without a ledger', () => {
    const { unmount } = render(<ApprovalCard card={card({ trust: {
      unavailable: 'ledger unreachable', line: 'Trust: unavailable (ledger unreachable)' } })} />)
    expect(screen.getByTestId('approval-trust')).toHaveTextContent('Trust: unavailable')
    unmount()
    render(<ApprovalCard card={card()} />)
    expect(screen.queryByTestId('approval-trust')).toBeNull()
  })

  it('approves with the card hash and reports execution truthfully', async () => {
    mockFetch(200, { ok: true, executed: true, status: 'approved',
      verification_status: 'monitoring', queue_id: 41 })
    const onDecided = vi.fn()
    render(<ApprovalCard card={card()} onDecided={onDecided} />)
    fireEvent.click(screen.getByTestId('approval-approve'))
    await waitFor(() => expect(onDecided).toHaveBeenCalled())
    const [url, init] = globalThis.fetch.mock.calls[0]
    expect(url).toBe('/api/v1/approvals/41/approve?database=orders')
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body)).toEqual({ card_hash: 'hash-41' })
    expect(toast.success).toHaveBeenCalledWith(
      expect.stringContaining('executed; verification: monitoring'))
  })

  it('says when an approval did not run', async () => {
    mockFetch(200, { ok: false, executed: false, status: 'failed',
      error: 'lock timeout' })
    render(<ApprovalCard card={card()} onDecided={vi.fn()} />)
    fireEvent.click(screen.getByTestId('approval-approve'))
    await waitFor(() => expect(toast.error).toHaveBeenCalled())
    expect(toast.error.mock.calls[0][0]).toContain('did not run')
    expect(toast.error.mock.calls[0][0]).toContain('lock timeout')
    expect(toast.success).not.toHaveBeenCalled()
  })

  it('refuses a stale card and asks to reload', async () => {
    mockFetch(409, { error: 'changed', code: 'content_changed' })
    const onDecided = vi.fn()
    render(<ApprovalCard card={card()} onDecided={onDecided} />)
    fireEvent.click(screen.getByTestId('approval-approve'))
    await waitFor(() => expect(toast.error).toHaveBeenCalled())
    expect(toast.error.mock.calls[0][0]).toContain('changed after this card was shown')
    expect(onDecided).toHaveBeenCalled()
  })

  it('needs a reason to reject and sends it', async () => {
    mockFetch(200, { ok: true, status: 'rejected' })
    const onDecided = vi.fn()
    render(<ApprovalCard card={card()} onDecided={onDecided} />)
    fireEvent.click(screen.getByTestId('approval-reject'))
    const confirm = screen.getByTestId('approval-reject-confirm')
    expect(confirm).toBeDisabled()
    fireEvent.change(screen.getByTestId('approval-reject-reason'),
      { target: { value: '  app owns this index ' } })
    expect(confirm).not.toBeDisabled()
    fireEvent.click(confirm)
    await waitFor(() => expect(onDecided).toHaveBeenCalled())
    const [url, init] = globalThis.fetch.mock.calls[0]
    expect(url).toBe('/api/v1/approvals/41/reject?database=orders')
    expect(JSON.parse(init.body)).toEqual({ reason: 'app owns this index',
      card_hash: 'hash-41' })
    expect(toast.success).toHaveBeenCalledWith(expect.stringContaining('rejected'))
  })

  it('snoozes for the chosen hours with a reason', async () => {
    mockFetch(200, { ok: true, status: 'snoozed' })
    render(<ApprovalCard card={card()} onDecided={vi.fn()} />)
    fireEvent.click(screen.getByTestId('approval-snooze'))
    fireEvent.change(screen.getByTestId('approval-snooze-hours'), { target: { value: '24' } })
    fireEvent.change(screen.getByTestId('approval-snooze-reason'),
      { target: { value: 'after the release' } })
    fireEvent.click(screen.getByTestId('approval-snooze-confirm'))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
    const [url, init] = globalThis.fetch.mock.calls[0]
    expect(url).toBe('/api/v1/approvals/41/snooze?database=orders')
    expect(JSON.parse(init.body)).toEqual({ hours: 24, reason: 'after the release' })
  })

  it('shows a snoozed card as snoozed', () => {
    render(<ApprovalCard card={card({ snoozed_until: '2026-10-03T18:00:00Z',
      snooze_reason: 'busy hours' })} />)
    expect(screen.getByTestId('approval-snoozed-badge')).toHaveTextContent('busy hours')
  })

  it('disables decisions while a request is in flight', async () => {
    let resolve
    globalThis.fetch = vi.fn(() => new Promise(r => { resolve = r }))
    render(<ApprovalCard card={card()} onDecided={vi.fn()} />)
    fireEvent.click(screen.getByTestId('approval-approve'))
    const actions = screen.getByTestId('approval-actions')
    expect(within(actions).getByTestId('approval-approve')).toBeDisabled()
    fireEvent.click(screen.getByTestId('approval-approve'))
    expect(globalThis.fetch).toHaveBeenCalledTimes(1)
    resolve({ ok: true, status: 200, json: async () => ({ ok: true, executed: true }) })
    await waitFor(() => expect(toast.success).toHaveBeenCalled())
  })

  it('reports a network failure', async () => {
    globalThis.fetch = vi.fn().mockRejectedValue(new Error('offline'))
    render(<ApprovalCard card={card()} onDecided={vi.fn()} />)
    fireEvent.click(screen.getByTestId('approval-approve'))
    await waitFor(() => expect(toast.error).toHaveBeenCalled())
    expect(toast.error.mock.calls[0][0]).toContain('offline')
  })
})

describe('approvalOutcomeMessage', () => {
  it('describes each outcome', () => {
    expect(approvalOutcomeMessage(200, { executed: true, verification_status: 'verified' }))
      .toEqual({ ok: true, text: expect.stringContaining('verification: verified') })
    expect(approvalOutcomeMessage(200, { executed: false, error: 'x' }).ok).toBe(false)
    expect(approvalOutcomeMessage(409, { code: 'not_pending', error: 'gone' }).text)
      .toContain('gone')
    expect(approvalOutcomeMessage(500, {}).text).toContain('Approve failed')
  })
})
