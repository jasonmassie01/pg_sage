import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ValuePage } from './ValuePage'

// market-04: the Value page carries the Cost card next to the toil hours,
// even before any toil credit exists.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))

vi.mock('../hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
}))

function respond(data) {
  api.useAPI.mockReturnValue({ data, loading: false, error: null, refetch: vi.fn() })
}

describe('ValuePage cost card', () => {
  beforeEach(() => api.useAPI.mockReset())

  it('shows priced cost with its source on an otherwise empty report', () => {
    respond({ cost: { available: true, rate_source: 'config', as_of: '2026-11-01',
      currency: 'USD', window_hours: 24, compute: 24, total: 24, idle_share: 0.5,
      top_queries: [] } })
    render(<ValuePage database="all" />)
    expect(screen.getByRole('region', { name: 'Cost' })).toHaveTextContent(
      'Rates from config, as of 2026-11-01.')
    expect(screen.getAllByText('$24.00')).toHaveLength(2)
    expect(screen.getByText('No verified savings yet')).toBeInTheDocument()
  })

  it('says rates are not configured', () => {
    respond({ cost: { available: false, reason: 'rates not configured' } })
    render(<ValuePage database="orders" />)
    expect(screen.getByRole('region', { name: 'Cost' })).toHaveTextContent(
      'Rates not configured')
  })
})
