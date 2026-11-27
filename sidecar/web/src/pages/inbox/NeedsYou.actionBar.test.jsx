import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { NeedsYou } from './NeedsYou'

// ux-12: an Inbox item's decision row is the same sticky action bar as an
// approval card's (ux-05a's .approval-action-bar, sticky under 768 px), so
// on a phone the decision stays in view at the bottom of a long item. The
// width rule itself is pinned in src/lint/contrast.test.js and by
// e2e/a11y-keyboard.spec.ts at 767 and 768 px.
// No concurrency tests: one list, single-threaded render.

vi.mock('../../hooks/useAPI', () => ({
  useAPI: () => ({ data: null, loading: false, error: null, refetch: vi.fn() }),
}))
vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const base = { database: 'orders', why: '', risk: 'safe', created_at: '2026-11-10T15:00:00Z',
  actionable: true }
const items = [
  { ...base, kind: 'change_approval', id: '4', title: 'Approve analyze_table 4',
    href: '#/actions?tab=pending&queue=4', source: 'sage.action_queue' },
  { ...base, kind: 'fact', id: '9', title: 'Confirm the fact', href: '#/facts' },
  { ...base, kind: 'autonomy_promotion', id: '2', title: 'Raise analyze_table',
    href: '#/autonomy', actionable: false },
]

describe('Inbox item action bar', () => {
  it('puts each item decision row in the sticky action bar', () => {
    render(<NeedsYou items={items} onDecided={vi.fn()} />)
    const change = screen.getByTestId('inbox-item-change_approval-4')
    const bar = within(change).getByTestId('inbox-decision-bar')
    expect(bar).toHaveClass('approval-action-bar')
    expect(within(bar).getByRole('button', { name: 'Review' })).toBeInTheDocument()
    expect(within(bar).getByRole('link', { name: 'Open' })).toHaveAttribute('href',
      '#/actions?tab=pending&queue=4')
    const fact = within(screen.getByTestId('inbox-item-fact-9')).getByTestId('inbox-decision-bar')
    expect(fact).toHaveClass('approval-action-bar')
    expect(within(fact).getByRole('button', { name: 'Confirm' })).toBeInTheDocument()
  })

  it('keeps the bar for an item the role can only view', () => {
    render(<NeedsYou items={items} onDecided={vi.fn()} />)
    const item = screen.getByTestId('inbox-item-autonomy_promotion-2')
    const bar = within(item).getByTestId('inbox-decision-bar')
    expect(bar).toHaveClass('approval-action-bar')
    expect(within(bar).queryByRole('button')).toBeNull()
    expect(bar).toHaveTextContent('an admin decides this')
  })
})

describe('Inbox item action bar while reviewing', () => {
  it('hands the bar to the approval card while the review is open', () => {
    render(<NeedsYou items={items} onDecided={vi.fn()} />)
    const change = screen.getByTestId('inbox-item-change_approval-4')
    const bar = within(change).getByTestId('inbox-decision-bar')
    fireEvent.click(within(bar).getByRole('button', { name: 'Review' }))
    expect(bar).not.toHaveClass('approval-action-bar')
    expect(within(bar).getByRole('button', { name: 'Hide review' })).toBeInTheDocument()
    fireEvent.click(within(bar).getByRole('button', { name: 'Hide review' }))
    expect(bar).toHaveClass('approval-action-bar')
  })
})
