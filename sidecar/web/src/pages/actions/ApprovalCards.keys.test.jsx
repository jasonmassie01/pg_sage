import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { PendingCardsView } from './ApprovalCards'

// ux-05a win 24: on the Needs your review card list, j and k move the
// selection, a asks to approve the selected card (through a confirm that
// names it, then the card's own approve request), r opens its reason field
// and s its snooze options. Keys never act while typing or behind a dialog.

const toast = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }))
vi.mock('../../components/Toast', () => ({ useToast: () => toast }))

function card(id, overrides = {}) {
  return {
    queue_id: id, database: 'orders', title: `Analyze table t${id}`,
    action_type: 'analyze_table', status: 'pending', targets: [],
    evidence: [], rationale: null, predicted_effect: {}, sql: `ANALYZE public.t${id}`,
    rollback: { class: 'no_rollback_needed', sql: '', note: 'Nothing to undo' },
    risk: { tier: 'moderate', guardrails: [], post_checks: [] },
    why_approval: [{ code: 'trust_level', text: 'Trust level is advisory' }],
    expires_at: '2099-01-01T00:00:00Z', card_hash: `hash-${id}`,
    ...overrides,
  }
}

const three = [card(41), card(42), card(43)]

let release
function stubFetch({ hold = false } = {}) {
  const mock = vi.fn(() => new Promise(resolve => {
    const done = () => resolve({ ok: true, status: 200,
      json: async () => ({ ok: true, executed: true, verification_status: 'pending' }) })
    if (hold) release = done
    else done()
  }))
  vi.stubGlobal('fetch', mock)
  return mock
}

function renderCards(props = {}) {
  const onDecided = vi.fn()
  render(<PendingCardsView cards={three} errors={[]} onShowTable={vi.fn()}
    onDecided={onDecided} {...props} />)
  return onDecided
}

const selectedTitle = () => {
  const wrapper = document.querySelector('[data-selected="true"]')
  return wrapper && within(wrapper).getByTestId('approval-card-title').textContent
}
const posts = mock => mock.mock.calls.filter(([, init]) => init?.method === 'POST')

beforeEach(() => { Object.values(toast).forEach(fn => fn.mockClear()) })
afterEach(() => { vi.unstubAllGlobals(); release = undefined })

describe('approval list keys: selection', () => {
  it('starts on the first card and j, k move one card at a time', async () => {
    stubFetch()
    renderCards()
    const user = userEvent.setup()
    expect(selectedTitle()).toBe('Analyze table t41')
    await user.keyboard('j')
    expect(selectedTitle()).toBe('Analyze table t42')
    await user.keyboard('jj')
    expect(selectedTitle()).toBe('Analyze table t43')
    await user.keyboard('kkkk')
    expect(selectedTitle()).toBe('Analyze table t41')
  })

  it('starts on the card a queue= link focused', () => {
    stubFetch()
    renderCards({ queueFocus: '42' })
    expect(selectedTitle()).toBe('Analyze table t42')
  })

  it('does nothing on an empty list', async () => {
    const mock = stubFetch()
    renderCards({ cards: [] })
    await userEvent.setup().keyboard('jas')
    expect(screen.getByTestId('approval-cards-empty')).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(mock).not.toHaveBeenCalled()
  })

  it('keeps a valid selection when the list shrinks', async () => {
    stubFetch()
    const { rerender } = render(<PendingCardsView cards={three} errors={[]}
      onShowTable={vi.fn()} onDecided={vi.fn()} />)
    await userEvent.setup().keyboard('jj')
    rerender(<PendingCardsView cards={three.slice(0, 2)} errors={[]}
      onShowTable={vi.fn()} onDecided={vi.fn()} />)
    expect(selectedTitle()).toBe('Analyze table t42')
  })
})

describe('approval list keys: approve', () => {
  it('j j a approves the third card through the confirm', async () => {
    const mock = stubFetch()
    const onDecided = renderCards()
    const user = userEvent.setup()
    await user.keyboard('jja')
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('heading')).toHaveTextContent('Approve: Analyze table t43')
    expect(within(dialog).getByTestId('approval-key-confirm-sql'))
      .toHaveTextContent('ANALYZE public.t43')
    expect(posts(mock)).toHaveLength(0)
    await act(async () => {
      fireEvent.click(within(dialog).getByTestId('approval-key-confirm-confirm'))
    })
    await waitFor(() => expect(onDecided).toHaveBeenCalledTimes(1))
    expect(posts(mock)).toHaveLength(1)
    const [url, init] = posts(mock)[0]
    expect(url).toBe('/api/v1/approvals/43/approve?database=orders')
    expect(JSON.parse(init.body)).toEqual({ card_hash: 'hash-43' })
    expect(toast.success).toHaveBeenCalledWith('Approved and executed; verification: pending')
  })

  it('cancelling the confirm sends nothing', async () => {
    const mock = stubFetch()
    renderCards()
    await userEvent.setup().keyboard('a')
    const dialog = await screen.findByRole('dialog')
    await act(async () => {
      fireEvent.click(within(dialog).getByTestId('approval-key-confirm-cancel'))
    })
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(mock).not.toHaveBeenCalled()
  })

  it('a on a card that cannot run says why and opens nothing', async () => {
    const mock = stubFetch()
    renderCards({ modeForDatabase: () => 'observation' })
    await userEvent.setup().keyboard('a')
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(toast.error).toHaveBeenCalledWith(
      'Analyze table t41 cannot be approved now: '
      + 'Database changes are disabled in Observation mode.')
    expect(mock).not.toHaveBeenCalled()
  })

  it('a pressed in the reject reason field types "a" and approves nothing', async () => {
    const mock = stubFetch()
    renderCards()
    const user = userEvent.setup()
    await user.keyboard('r')
    const reason = screen.getByTestId('approval-reject-reason')
    expect(reason).toHaveFocus()
    await user.keyboard('a')
    expect(reason).toHaveValue('a')
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(mock).not.toHaveBeenCalled()
  })

  it('ignores a with Ctrl held', async () => {
    const mock = stubFetch()
    renderCards()
    await userEvent.setup().keyboard('{Control>}a{/Control}')
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(mock).not.toHaveBeenCalled()
  })
})

describe('approval list keys: busy and dialogs', () => {
  it('a twice and confirm twice send one approve', async () => {
    const mock = stubFetch({ hold: true })
    renderCards()
    const user = userEvent.setup()
    await user.keyboard('aa')
    expect(screen.getAllByRole('dialog')).toHaveLength(1)
    const confirm = screen.getByTestId('approval-key-confirm-confirm')
    await act(async () => { fireEvent.click(confirm) })
    await act(async () => { fireEvent.click(confirm) })
    expect(confirm).toBeDisabled()
    expect(posts(mock)).toHaveLength(1)
    await act(async () => { release() })
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(posts(mock)).toHaveLength(1)
  })

  it('does not move the selection while the confirm is open', async () => {
    stubFetch()
    renderCards()
    const user = userEvent.setup()
    await user.keyboard('a')
    await screen.findByRole('dialog')
    fireEvent.keyDown(window, { key: 'j' })
    expect(selectedTitle()).toBe('Analyze table t41')
  })
})

describe('approval list keys: reject and snooze', () => {
  it('r opens the selected card reason field, focused and empty', async () => {
    stubFetch()
    renderCards()
    const user = userEvent.setup()
    await user.keyboard('jr')
    const cards = screen.getAllByTestId('approval-card')
    const reason = within(cards[1]).getByTestId('approval-reject-reason')
    expect(reason).toHaveFocus()
    expect(reason).toHaveValue('')
    expect(within(cards[0]).queryByTestId('approval-reject-reason')).toBeNull()
  })

  it('r on an open reason field keeps it open', async () => {
    stubFetch()
    renderCards()
    const user = userEvent.setup()
    await user.keyboard('r')
    screen.getByTestId('approval-reject-reason').blur()
    await user.keyboard('r')
    expect(screen.getByTestId('approval-reject-reason')).toHaveFocus()
  })

  it('s opens the selected card snooze options and sends nothing', async () => {
    const mock = stubFetch()
    renderCards()
    await userEvent.setup().keyboard('jjs')
    const cards = screen.getAllByTestId('approval-card')
    expect(within(cards[2]).getByTestId('approval-snooze-hours')).toHaveFocus()
    expect(within(cards[0]).queryByTestId('approval-snooze-hours')).toBeNull()
    expect(mock).not.toHaveBeenCalled()
  })
})
