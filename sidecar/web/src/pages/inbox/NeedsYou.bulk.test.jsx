import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { NeedsYou } from './NeedsYou'

// ux-07b on the Inbox: safe-tier change approvals get a checkbox and the
// "Select all <class> on <db>" helper. The confirm reads each selected card
// first, so it lists the SQL the person approves and sends that card's
// hash; a card that is no longer safe tier is left out. The approval keys
// (j, k, a, r, s, x, Enter) drive this list too.
// No concurrent access tests: one list and one bulk request at a time.

const api = vi.hoisted(() => ({ useAPI: vi.fn(), toast: { success: vi.fn(), error: vi.fn() } }))
vi.mock('../../hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))
vi.mock('../../components/Toast', () => ({ useToast: () => api.toast }))

const item = (id, extra = {}) => ({ kind: 'change_approval', id: String(id), database: 'orders',
  title: `Approve analyze_table ${id}`, why: '', risk: 'safe', class: 'analyze_table',
  created_at: '2026-11-10T15:00:00Z', href: `#/actions?tab=pending&queue=${id}`,
  source: 'sage.action_queue', actionable: true, ...extra })

const cardOf = (id, tier = 'safe') => ({ queue_id: id, database: 'orders',
  title: `Analyze table t${id}`, action_type: 'analyze_table', status: 'pending',
  targets: [], evidence: [], rationale: null, predicted_effect: {},
  sql: `ANALYZE public.t${id}`, rollback: { class: 'no_rollback_needed', sql: '', note: '' },
  risk: { tier, guardrails: [], post_checks: [] }, why_approval: [],
  expires_at: '2099-01-01T00:00:00Z', card_hash: `hash-${id}` })

function stubFetch({ tiers = {}, bulk = { results: [], approved: 0 } } = {}) {
  const mock = vi.fn(async (url, init) => {
    const u = String(url)
    if (init?.method === 'POST' && u.includes('bulk-approve')) {
      return { ok: true, status: 200, json: async () => bulk }
    }
    const id = Number(u.match(/approvals\/(\d+)/)?.[1])
    return { ok: true, status: 200,
      json: async () => ({ card: cardOf(id, tiers[id] || 'safe'), eligible: true }) }
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

const bulkPosts = mock => mock.mock.calls
  .filter(([url, init]) => init?.method === 'POST' && String(url).includes('bulk-approve'))

function renderList(items) {
  const onDecided = vi.fn()
  render(<NeedsYou items={items} onDecided={onDecided} />)
  return onDecided
}

const box = id => screen.getByTestId(`bulk-select-orders-${id}`)

beforeEach(() => {
  api.useAPI.mockImplementation(url => {
    const id = Number(String(url).match(/approvals\/(\d+)/)?.[1])
    return { data: url ? { card: cardOf(id), eligible: true } : null, loading: false,
      error: null, refetch: vi.fn() }
  })
  Object.values(api.toast).forEach(fn => fn.mockClear())
})
afterEach(() => { vi.unstubAllGlobals() })

describe('Inbox bulk selection', () => {
  it('offers a checkbox only on safe change approvals the reader can decide', () => {
    stubFetch()
    renderList([item(1), item(2, { risk: 'moderate' }), item(3, { actionable: false }),
      { ...item(4), kind: 'fact', title: 'a fact' }])
    expect(box(1)).toBeEnabled()
    expect(box(2)).toBeDisabled()
    expect(screen.queryByTestId('bulk-select-orders-3')).toBeNull()
    expect(screen.queryByTestId('bulk-select-orders-4')).toBeNull()
  })

  it('has no bulk bar when nothing can be bulk approved', () => {
    stubFetch()
    renderList([item(2, { risk: 'high' })])
    expect(screen.queryByTestId('bulk-approve-bar')).toBeNull()
  })

  it('selects every safe item of one class on one database', async () => {
    stubFetch()
    renderList([item(1), item(2), item(3, { class: 'vacuum_table' })])
    await userEvent.setup().click(
      screen.getByRole('button', { name: 'Select all analyze_table on orders (2)' }))
    expect(box(1)).toBeChecked()
    expect(box(2)).toBeChecked()
    expect(box(3)).not.toBeChecked()
  })
})

describe('Inbox bulk approve', () => {
  it('reads each card, lists its SQL and sends its hash from the Inbox', async () => {
    const mock = stubFetch({ bulk: { results: [
      { id: 1, database: 'orders', status: 'approved' },
      { id: 2, database: 'orders', status: 'approved' }], approved: 2 } })
    const onDecided = renderList([item(1), item(2)])
    const user = userEvent.setup()
    await user.click(box(1))
    await user.click(box(2))
    await user.click(screen.getByTestId('bulk-approve-button'))
    const dialog = await screen.findByTestId('bulk-approve-dialog')
    const rows = await within(dialog).findAllByTestId('bulk-approve-item')
    expect(rows.map(r => r.textContent)).toEqual([
      expect.stringContaining('ANALYZE public.t1'), expect.stringContaining('ANALYZE public.t2')])
    await user.click(within(dialog).getByTestId('bulk-approve-dialog-confirm'))
    await waitFor(() => expect(onDecided).toHaveBeenCalledTimes(1))
    const [[, init]] = bulkPosts(mock)
    expect(init.headers['X-Sage-Surface']).toBe('inbox')
    expect(JSON.parse(init.body)).toEqual({ items: [
      { id: 1, database: 'orders', card_hash: 'hash-1' },
      { id: 2, database: 'orders', card_hash: 'hash-2' }] })
  })

  it('leaves out a card that is no longer safe tier and says so', async () => {
    const mock = stubFetch({ tiers: { 2: 'moderate' }, bulk: { results: [
      { id: 1, database: 'orders', status: 'approved' }], approved: 1 } })
    renderList([item(1), item(2)])
    const user = userEvent.setup()
    await user.click(box(1))
    await user.click(box(2))
    await user.click(screen.getByTestId('bulk-approve-button'))
    const dialog = await screen.findByTestId('bulk-approve-dialog')
    expect(await within(dialog).findByTestId('bulk-approve-skipped'))
      .toHaveTextContent(/Analyze table t2.*not safe tier/)
    expect(within(dialog).getAllByTestId('bulk-approve-item')).toHaveLength(1)
    await user.click(within(dialog).getByTestId('bulk-approve-dialog-confirm'))
    await waitFor(() => expect(bulkPosts(mock)).toHaveLength(1))
    expect(JSON.parse(bulkPosts(mock)[0][1].body).items.map(i => i.id)).toEqual([1])
  })
})

describe('Inbox keys', () => {
  it('j moves, x selects and Enter opens the review in place', async () => {
    stubFetch()
    renderList([item(1), item(2)])
    const user = userEvent.setup()
    await user.keyboard('jx')
    expect(box(2)).toBeChecked()
    expect(box(1)).not.toBeChecked()
    await user.keyboard('{Enter}')
    const li = screen.getByTestId('inbox-item-change_approval-2')
    expect(await within(li).findByTestId('approval-card')).toBeInTheDocument()
  })

  it('a opens the review and asks to approve the selected item', async () => {
    const mock = stubFetch()
    renderList([item(1, { risk: 'moderate' })])
    await userEvent.setup().keyboard('a')
    const confirm = await screen.findByRole('dialog', { name: 'Approve: Analyze table t1' })
    expect(confirm).toBeVisible()
    expect(mock.mock.calls.filter(([, init]) => init?.method === 'POST')).toEqual([])
  })
})
