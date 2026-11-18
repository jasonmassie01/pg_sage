import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { PendingCardsView } from './ApprovalCards'

// ux-07b: on the Waiting on you cards, safe-tier cards can be selected
// (checkbox or x) and approved together. The confirm lists every SQL
// statement with its database; each item is decided on its own by the
// server, so a refused item stays selected with its reason. Enter opens
// the selected card. Keys never act behind a dialog.
// No concurrent access tests: one bulk request at a time (the button is
// busy until it returns); the server-side overlap is a Go DB test.

const toast = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }))
vi.mock('../../components/Toast', () => ({ useToast: () => toast }))

function card(id, tier = 'safe', overrides = {}) {
  return {
    queue_id: id, database: 'orders', title: `Analyze table t${id}`,
    action_type: 'analyze_table', status: 'pending', targets: [],
    evidence: [], rationale: null, predicted_effect: {}, sql: `ANALYZE public.t${id}`,
    rollback: { class: 'no_rollback_needed', sql: '', note: 'Nothing to undo' },
    risk: { tier, guardrails: [], post_checks: [] },
    why_approval: [{ code: 'trust_level', text: 'Trust level is advisory' }],
    expires_at: '2099-01-01T00:00:00Z', card_hash: `hash-${id}`,
    ...overrides,
  }
}

const safeThree = [card(41), card(42), card(43)]

function result(id, status, reason) {
  return { id, database: 'orders', status, executed: status === 'approved',
    ...(reason ? { code: 'not_eligible', reason } : {}) }
}

function stubBulk(body, status = 200) {
  const mock = vi.fn(async (url, init) => {
    if (init?.method === 'POST' && String(url).includes('bulk-approve')) {
      return { ok: status < 300, status, json: async () => body }
    }
    return { ok: true, status: 200, json: async () => ({}) }
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

const bulkPosts = mock => mock.mock.calls
  .filter(([url, init]) => init?.method === 'POST' && String(url).includes('bulk-approve'))
  .map(([, init]) => JSON.parse(init.body))

function renderCards(cards = safeThree) {
  const onDecided = vi.fn()
  const view = render(<PendingCardsView cards={cards} errors={[]} onShowTable={vi.fn()}
    onDecided={onDecided} />)
  return { onDecided, view }
}

const box = id => screen.getByTestId(`bulk-select-orders-${id}`)
const bulkButton = () => screen.getByTestId('bulk-approve-button')

async function approveSelected(user) {
  await user.click(bulkButton())
  const dialog = await screen.findByTestId('bulk-approve-dialog')
  await user.click(within(dialog).getByTestId('bulk-approve-dialog-confirm'))
  return dialog
}

beforeEach(() => { Object.values(toast).forEach(fn => fn.mockClear()) })
afterEach(() => { vi.unstubAllGlobals() })

describe('bulk selection', () => {
  it('offers a checkbox only on safe-tier cards', () => {
    stubBulk({})
    renderCards([card(41), card(42, 'moderate'), card(43, 'high')])
    expect(box(41)).toBeEnabled()
    expect(box(42)).toBeDisabled()
    expect(box(43)).toBeDisabled()
    expect(box(42)).toHaveAccessibleDescription(/safe/i)
  })

  it('disables the bulk button while nothing is selected', async () => {
    stubBulk({})
    renderCards()
    expect(bulkButton()).toBeDisabled()
    expect(bulkButton()).toHaveTextContent('Approve selected (0)')
    const user = userEvent.setup()
    await user.click(box(42))
    expect(bulkButton()).toBeEnabled()
    expect(bulkButton()).toHaveTextContent('Approve selected (1)')
  })

  it('selects every safe card of one class on one database', async () => {
    stubBulk({})
    renderCards([card(41), card(42), card(43, 'moderate'),
      card(44, 'safe', { action_type: 'vacuum_table' })])
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Select all analyze_table on orders (2)' }))
    expect(box(41)).toBeChecked()
    expect(box(42)).toBeChecked()
    expect(box(43)).not.toBeChecked()
    expect(box(44)).not.toBeChecked()
  })
})

describe('bulk approve', () => {
  it('lists each SQL and database, then sends each card with its own hash', async () => {
    const mock = stubBulk({ results: [result(41, 'approved'), result(42, 'approved'),
      result(43, 'approved')], approved: 3, scheduled: 0, refused: 0, failed: 0 })
    const { onDecided } = renderCards()
    const user = userEvent.setup()
    for (const id of [41, 42, 43]) await user.click(box(id))
    await user.click(bulkButton())
    const dialog = await screen.findByTestId('bulk-approve-dialog')
    const rows = within(dialog).getAllByTestId('bulk-approve-item')
    expect(rows.map(r => r.textContent)).toEqual([41, 42, 43]
      .map(id => expect.stringContaining(`ANALYZE public.t${id}`)))
    for (const row of rows) expect(row).toHaveTextContent('orders')
    expect(bulkPosts(mock)).toEqual([])
    await user.click(within(dialog).getByTestId('bulk-approve-dialog-confirm'))
    await waitFor(() => expect(onDecided).toHaveBeenCalledTimes(1))
    expect(bulkPosts(mock)).toEqual([{ items: [41, 42, 43].map(id =>
      ({ id, database: 'orders', card_hash: `hash-${id}` })) }])
    expect(toast.success).toHaveBeenCalledWith('3 approved')
    expect(bulkButton()).toHaveTextContent('Approve selected (0)')
    for (const id of [41, 42, 43]) expect(box(id)).not.toBeChecked()
  })

  it('keeps a refused item selected and shows its reason', async () => {
    stubBulk({ results: [result(41, 'approved'),
      result(42, 'refused', 'the finding behind this action is resolved'),
      result(43, 'approved')], approved: 2, scheduled: 0, refused: 1, failed: 0 })
    renderCards()
    const user = userEvent.setup()
    for (const id of [41, 42, 43]) await user.click(box(id))
    await approveSelected(user)
    const refused = await screen.findByTestId('bulk-result-orders-42')
    expect(refused).toHaveTextContent('the finding behind this action is resolved')
    expect(refused).toHaveTextContent('Analyze table t42')
    expect(box(42)).toBeChecked()
    expect(box(41)).not.toBeChecked()
    expect(bulkButton()).toHaveTextContent('Approve selected (1)')
    expect(toast.success).toHaveBeenCalledWith('2 approved, 1 refused')
  })

  it('shows a refused request inside the dialog and keeps the selection', async () => {
    stubBulk({ error: 'bulk approve takes at most 50 items', code: 'invalid_request' }, 400)
    const { onDecided } = renderCards()
    const user = userEvent.setup()
    await user.click(box(41))
    const dialog = await approveSelected(user)
    expect(await within(dialog).findByRole('alert'))
      .toHaveTextContent('bulk approve takes at most 50 items')
    expect(box(41)).toBeChecked()
    expect(onDecided).not.toHaveBeenCalled()
  })
})

describe('bulk keys', () => {
  it('x selects the selected safe card and clears it again', async () => {
    stubBulk({})
    renderCards()
    const user = userEvent.setup()
    await user.keyboard('jx')
    expect(box(42)).toBeChecked()
    expect(box(41)).not.toBeChecked()
    await user.keyboard('x')
    expect(box(42)).not.toBeChecked()
  })

  it('x on a card that is not safe tier selects nothing and says why', async () => {
    stubBulk({})
    renderCards([card(41, 'moderate')])
    await userEvent.setup().keyboard('x')
    expect(box(41)).not.toBeChecked()
    expect(toast.error).toHaveBeenCalledWith(expect.stringMatching(/safe tier only/))
  })

  it('Enter opens the selected card', async () => {
    stubBulk({})
    renderCards()
    const user = userEvent.setup()
    await user.keyboard('j{Enter}')
    const opened = document.querySelector('[data-selected="true"]')
    expect(opened).toHaveFocus()
    expect(within(opened).getByTestId('approval-card-title')).toHaveTextContent('t42')
  })

  it('a with cards selected opens the bulk confirm, not the card confirm', async () => {
    const mock = stubBulk({})
    renderCards()
    const user = userEvent.setup()
    await user.keyboard('xjx')
    await user.keyboard('a')
    expect(await screen.findByRole('dialog', { name: 'Approve 2 safe changes' })).toBeVisible()
    expect([...document.querySelectorAll('dialog[open]')].map(d => d.dataset.testid))
      .toEqual(['bulk-approve-dialog'])
    expect(bulkPosts(mock)).toEqual([])
  })

  it('does nothing while the bulk dialog is open', async () => {
    const mock = stubBulk({})
    renderCards()
    const user = userEvent.setup()
    await user.keyboard('x')
    await user.click(bulkButton())
    await screen.findByTestId('bulk-approve-dialog')
    await user.keyboard('jxa')
    expect(box(41)).toBeChecked()
    expect(box(42)).not.toBeChecked()
    expect([...document.querySelectorAll('dialog[open]')].map(d => d.dataset.testid))
      .toEqual(['bulk-approve-dialog'])
    expect(bulkPosts(mock)).toEqual([])
  })
})
