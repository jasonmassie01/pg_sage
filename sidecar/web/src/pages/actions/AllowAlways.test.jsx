import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApprovalCard } from './ApprovalCard'
import { allowAlwaysOutcomeMessage } from './approvalOutcome'

// ux-07a: the second affirmative button appears only when the server
// offers it, confirms before it runs, and posts the card hash and the
// card's own class to approve-and-allow. No concurrency tests here: the
// one-decision-in-flight rule is ApprovalActions' and is tested there.

const toast = { success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() }
vi.mock('../../components/Toast', () => ({ useToast: () => toast }))

const offered = { family: 'hygiene', class: 'analyze', level: 'L2', allow_always: true,
  allow_always_label: 'Approve, and stop asking for analyze on orders',
  approval_history: { approvals: 14, required: 10, qualifies: true },
  line: 'Trust: hygiene/analyze at L2' }

function card(trust) {
  return { queue_id: 41, database: 'orders', title: 'ANALYZE public.orders',
    status: 'pending', sql: 'ANALYZE public.orders', risk: { tier: 'safe' },
    rollback: {}, card_hash: 'hash-41', trust }
}

function mockFetch(status, body) {
  globalThis.fetch = vi.fn().mockResolvedValue({ ok: status < 300, status,
    json: async () => body })
}

beforeEach(() => Object.values(toast).forEach(fn => fn.mockClear()))
afterEach(() => { delete globalThis.fetch })

describe('AllowAlways', () => {
  it('is hidden unless the server offers it', () => {
    for (const trust of [undefined, null, { ...offered, allow_always: false }]) {
      const { unmount } = render(<ApprovalCard card={card(trust)} />)
      expect(screen.queryByTestId('approval-allow-always')).toBeNull()
      unmount()
    }
  })

  it('confirms, then posts the hash and the card class', async () => {
    mockFetch(200, { ok: true, executed: true, verification_status: 'verified',
      allow_always: true, autonomy_level: 'L3' })
    render(<ApprovalCard card={card(offered)} />)
    const button = screen.getByTestId('approval-allow-always')
    expect(button).toHaveTextContent('Approve, and stop asking for analyze on orders')
    fireEvent.click(button)
    expect(globalThis.fetch).not.toHaveBeenCalled()
    expect(screen.getByText(/You approved analyze 14 times/)).toBeInTheDocument()
    fireEvent.click(screen.getByTestId('approval-allow-always-confirm-confirm'))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    const [url, init] = globalThis.fetch.mock.calls[0]
    expect(url).toBe('/api/v1/approvals/41/approve-and-allow?database=orders')
    expect(JSON.parse(init.body)).toEqual({ card_hash: 'hash-41', family: 'hygiene',
      class: 'analyze' })
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith(
      expect.stringContaining('runs unattended (L3)')))
  })

  it('is disabled when the action cannot be approved', () => {
    render(<ApprovalCard card={card(offered)} eligible={false} />)
    expect(screen.getByTestId('approval-allow-always')).toBeDisabled()
  })
})

describe('allowAlwaysOutcomeMessage', () => {
  it('says when the approval stood and autonomy did not change', () => {
    const msg = allowAlwaysOutcomeMessage(200, { ok: true, executed: true,
      allow_always: false,
      autonomy_error: 'approved; autonomy unchanged: autonomy proposal is not pending' })
    expect(msg.ok).toBe(false)
    expect(msg.text).toContain('Approved and executed')
    expect(msg.text).toContain('approved; autonomy unchanged: autonomy proposal is not pending')
  })

  it('passes a refused request through', () => {
    const msg = allowAlwaysOutcomeMessage(409, { code: 'content_changed', error: 'x' })
    expect(msg).toEqual({ ok: false, text: expect.stringContaining('changed') })
    expect(allowAlwaysOutcomeMessage(403, { error: 'a promotion needs a human approver' }))
      .toEqual({ ok: false, text: 'a promotion needs a human approver' })
    expect(allowAlwaysOutcomeMessage(200, null).ok).toBe(false)
  })
})
