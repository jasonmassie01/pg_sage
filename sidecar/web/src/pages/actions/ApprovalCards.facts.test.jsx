import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { PendingCardsView } from './ApprovalCards'

// Each approval card shows the binding facts about its targets, so the
// reviewer sees "Bound by fact #12" before approving, and can confirm or
// reject a proposed fact right there (the card list then refreshes).

vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const card = {
  queue_id: 41, database: 'orders', title: 'Stale statistics on public.orders',
  action_type: 'analyze_table', status: 'pending', targets: ['public.orders'],
  evidence: [], rationale: null, predicted_effect: {}, sql: 'ANALYZE public.orders',
  rollback: { class: 'no_rollback_needed', sql: '', note: 'Nothing to undo' },
  risk: { tier: 'safe', guardrails: [], post_checks: [] },
  why_approval: [{ code: 'trust_level', text: 'Trust level is advisory' }],
  expires_at: '2026-10-04T11:00:00Z', card_hash: 'h',
}

const bound = { id: 12, database: 'orders', status: 'confirmed',
  summary: 'public.orders is append-only',
  provenance: 'fact #12, confirmed by alice@example.com on 2026-10-04' }
const pending = { id: 13, database: 'orders', status: 'proposed',
  summary: 'public.orders has a batch window: daily 01:00-03:00 UTC',
  provenance: 'fact #13, proposed by model:advisor on 2026-10-03' }

function response(status, body) {
  return { ok: status >= 200 && status < 300, status, statusText: `status ${status}`,
    json: async () => body }
}

function stubFetch() {
  const mock = vi.fn(async (url, init = {}) => {
    if ((init.method || 'GET') === 'POST') return response(200, { fact: pending })
    return response(200, { database: 'orders', objects: {
      'public.orders': { confirmed: [bound], proposed: [pending] } } })
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

afterEach(() => vi.unstubAllGlobals())

describe('approval cards with binding facts', () => {
  it('shows the facts about the card targets inside the card', async () => {
    const mock = stubFetch()
    render(<PendingCardsView cards={[card]} errors={[]} onShowTable={vi.fn()}
      onDecided={vi.fn()} />)
    const approval = screen.getByTestId('approval-card')
    const chip = await within(approval).findByTestId('fact-bound-12')
    expect(chip).toHaveTextContent('Bound by fact #12: public.orders is append-only')
    expect(mock.mock.calls[0][0]).toBe(
      '/api/v1/facts/match?database=orders&object=public.orders')
    expect(within(approval).getByTestId('fact-proposed-13')).toBeInTheDocument()
  })

  it('refreshes the cards after a fact is confirmed from a card', async () => {
    const mock = stubFetch()
    const onDecided = vi.fn()
    render(<PendingCardsView cards={[card]} errors={[]} onShowTable={vi.fn()}
      onDecided={onDecided} />)
    fireEvent.click(await screen.findByTestId('fact-badge-confirm-13'))
    await waitFor(() => expect(onDecided).toHaveBeenCalledTimes(1))
    const post = mock.mock.calls.find(([, init]) => init?.method === 'POST')
    expect(post[0]).toBe('/api/v1/facts/13/confirm?database=orders')
  })

  it('shows no fact section for a card without targets', async () => {
    const mock = stubFetch()
    render(<PendingCardsView cards={[{ ...card, targets: [] }]} errors={[]}
      onShowTable={vi.fn()} onDecided={vi.fn()} />)
    expect(screen.getByTestId('approval-card')).toBeInTheDocument()
    expect(screen.queryByTestId('fact-badges')).toBeNull()
    expect(mock).not.toHaveBeenCalled()
  })
})
