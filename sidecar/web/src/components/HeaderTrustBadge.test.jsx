import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useAPI } from '../hooks/useAPI'
import { HeaderTrustBadge } from './HeaderTrustBadge'

// ux-06b: the header badge counts classes per autonomy mode, not the trust
// level; an emergency stop is named first; without modes it falls back to
// the level badge (dashboard-02).

vi.mock('../hooks/useAPI', () => ({ useAPI: vi.fn() }))

const fleet = [
  { name: 'orders', emergency_stopped: false, status: { trust_level: 'advisory' } },
  { name: 'billing', emergency_stopped: false, status: { trust_level: 'advisory' } },
]
const trust = { databases: [
  { database: 'orders', rows: [{ mode: 'asks_first' }, { mode: 'asks_first' },
    { mode: 'acts_in_window' }] },
] }

afterEach(() => vi.clearAllMocks())

describe('HeaderTrustBadge', () => {
  it('counts the fleet classes per mode', () => {
    useAPI.mockReturnValue({ data: trust })
    render(<HeaderTrustBadge rows={fleet} selectedDB="all" />)
    const badge = screen.getByTestId('autonomy-badge')
    expect(badge).toHaveTextContent('2 ask first · 1 act in window')
    expect(badge).toHaveAttribute('href', '#/autonomy')
    expect(useAPI.mock.calls[0][0]).toBe('/api/v1/trust')
    expect(screen.queryByTestId('trust-badge-advisory')).toBeNull()
  })

  it('reads only the selected database', () => {
    useAPI.mockReturnValue({ data: trust })
    render(<HeaderTrustBadge rows={fleet} selectedDB="orders" />)
    expect(useAPI.mock.calls[0][0]).toBe('/api/v1/trust?database=orders')
    expect(screen.getByTestId('autonomy-badge')).toHaveTextContent('2 ask first')
  })

  it('names an emergency stop before the counts', () => {
    useAPI.mockReturnValue({ data: trust })
    const stopped = [{ ...fleet[0], emergency_stopped: true }, fleet[1]]
    render(<HeaderTrustBadge rows={stopped} selectedDB="all" />)
    const badge = screen.getByTestId('autonomy-badge-stopped')
    expect(badge).toHaveTextContent('Emergency stop on 1 · 2 ask first · 1 act in window')
    expect(badge.getAttribute('title')).toContain('orders')
  })

  it('falls back to the level badge without modes', () => {
    for (const data of [null, { databases: [{ database: 'orders', rows: [{}] }] }]) {
      useAPI.mockReturnValue({ data })
      const { unmount } = render(<HeaderTrustBadge rows={fleet} selectedDB="all" />)
      expect(screen.queryByTestId('autonomy-badge')).toBeNull()
      expect(screen.getByTestId('trust-badge-advisory')).toBeInTheDocument()
      unmount()
    }
  })

  it('renders nothing without fleet rows', () => {
    useAPI.mockReturnValue({ data: trust })
    const { container } = render(<HeaderTrustBadge rows={[]} selectedDB="all" />)
    expect(container).toBeEmptyDOMElement()
  })
})
