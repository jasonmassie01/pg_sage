import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { CostCard } from './CostCard'
import { formatMoney } from '../lib/money'

// market-04: dollars show only with their rate source and date.
// No concurrent access tests: a pure render of one response.

const priced = {
  available: true,
  rate_source: 'config',
  as_of: '2026-11-01',
  currency: 'USD',
  window_hours: 168,
  compute: 84,
  storage: 1.5,
  total: 85.5,
  idle_share: 0.97,
  top_queries: [
    { database: 'orders', queryid: '-9100000000000000001', share: 0.75, cost: 63, currency: 'USD' },
    { database: 'billing db', queryid: '42', share: 0.25, cost: 21, currency: 'USD' },
  ],
}

describe('CostCard', () => {
  it('shows dollars with their rate source and date', () => {
    render(<CostCard cost={priced} />)
    expect(screen.getByTestId('cost-source')).toHaveTextContent(
      'Rates from config, as of 2026-11-01.')
    expect(screen.getByText('$85.50')).toBeInTheDocument()
    expect(screen.getByText('$84.00')).toBeInTheDocument()
    expect(screen.getByText('97.0%')).toBeInTheDocument()
  })

  it('links each query to the findings of its database and keeps exact ids', () => {
    render(<CostCard cost={priced} />)
    const table = screen.getByRole('table', { name: 'Top queries by cost' })
    const rows = within(table).getAllByRole('row')
    expect(rows).toHaveLength(3)
    expect(within(rows[1]).getByText('-9100000000000000001')).toBeInTheDocument()
    expect(within(rows[2]).getByRole('link', { name: 'billing db' }))
      .toHaveAttribute('href', '#/cases?database=billing+db')
    expect(within(rows[1]).getByText('$63.00')).toBeInTheDocument()
  })

  it('says rates are not configured and shows no figure', () => {
    render(<CostCard cost={{ available: false, reason: 'rates not configured' }} />)
    expect(screen.getByText(/Rates not configured/)).toBeInTheDocument()
    expect(screen.queryByText(/\$/)).not.toBeInTheDocument()
  })

  it('handles a missing section and other reasons', () => {
    const { rerender } = render(<CostCard cost={undefined} />)
    expect(screen.getByText(/Cost unavailable: unknown reason/)).toBeInTheDocument()
    rerender(<CostCard cost={{ available: false, reason: 'no configured rate matches' }} />)
    expect(screen.getByText(/Cost unavailable: no configured rate matches/)).toBeInTheDocument()
  })

  it('shows an empty top list and unknown figures without inventing values', () => {
    render(<CostCard cost={{ ...priced, top_queries: [], total: null, compute: null,
      currency: 'mixed', idle_share: null, storage: null, unpriced: ['scratch'] }} />)
    expect(screen.getByText('No query time recorded in this window.')).toBeInTheDocument()
    expect(screen.getAllByText('unknown').length).toBeGreaterThanOrEqual(3)
    expect(screen.getByText(/different currencies/)).toBeInTheDocument()
    expect(screen.getByText(/Not priced \(no rate\): scratch/)).toBeInTheDocument()
  })

  it('names a partial read', () => {
    render(<CostCard cost={{ ...priced, partial: true,
      unavailable: ['orders: connection refused'] }} />)
    expect(screen.getByRole('alert')).toHaveTextContent('orders: connection refused')
  })
})

describe('formatMoney', () => {
  it('formats known currencies and rejects non-numbers', () => {
    expect(formatMoney(1234.5, 'EUR')).toBe('€1,234.50')
    expect(formatMoney(-365, 'USD')).toBe('-$365.00')
    expect(formatMoney(null, 'USD')).toBe('unknown')
    expect(formatMoney('abc', 'USD')).toBe('unknown')
    expect(formatMoney(2, 'XXZ')).toMatch(/2\.00|XXZ/)
  })
})
