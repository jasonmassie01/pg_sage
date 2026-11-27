import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Actions } from './Actions'

// dashboard-07: the header picker scopes the Actions ledger only. The
// proposals and approval requests are current state, and say so.
// No concurrent access tests: one render, no shared state.

vi.mock('../context/TimeRangeContext', () => ({
  useTimeRange: () => ({ range: { label: 'Last 24h' } }),
}))
vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: vi.fn() }))
vi.mock('../components/Layout', () => ({ usePendingActionsRefetch: () => vi.fn() }))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))
vi.mock('../hooks/useAPI', () => ({
  withTimeRange: path => path,
  useAPI: () => ({ data: null, loading: false, error: null, refetch: vi.fn() }),
}))

const CAPTION = 'Current state, not time-scoped.'
const caption = () => screen.queryByTestId('actions-not-time-scoped')

function openTab(key) {
  render(<Actions database="orders" user={{ role: 'admin' }} />)
  fireEvent.click(screen.getByTestId(`actions-tab-${key}`))
}

describe('Actions time scope captions', () => {
  it('captions the proposals as not time-scoped', () => {
    openTab('recommendations')
    expect(caption()).toHaveTextContent(CAPTION)
  })

  it('captions the approval requests as not time-scoped', () => {
    openTab('pending')
    expect(caption()).toHaveTextContent(CAPTION)
  })

  it.each(['progress', 'executed'])('leaves the windowed %s ledger uncaptioned', key => {
    openTab(key)
    expect(caption()).toBeNull()
  })
})
