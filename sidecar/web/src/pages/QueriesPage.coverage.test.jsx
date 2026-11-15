import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { QueriesPage } from './QueriesPage'
import { stubFetch } from '../test/fetchStub'

// evid-01b: the one-line coverage notice of the long per-query history
// under the top statements heading. It speaks only when there is news
// (delta-only): below 90% covered, while learning, when statistics reset
// often, or when the coverage is unknown; it is silent when ok and above
// 90%, and when the coverage cannot be read.
// No concurrent access tests: polled GETs and local state only.

afterEach(() => vi.unstubAllGlobals())

const entry = (over = {}) => ({
  database: 'shop', status: 'ok', covered_exec_share: 0.87, epoch_breaks: 3,
  hours: 672, hours_missing: 0, dealloc_growth: 0, epoch_source_counters_only_hours: 0,
  oldest_hour: '2026-10-15T00:00:00Z', oldest_day: '2026-10-15', rollup_bytes: 52428800,
  ...over,
})

function renderWith(coverage, database = 'shop') {
  const fetch = stubFetch({
    '/api/v1/queries/top': { json: { database, coverage: 'full', order: 'load', queries: [] } },
    '/api/v1/wait-load': { json: { database, coverage: 'none', series: [], classes: [] } },
    '/api/v1/queries/coverage': coverage,
  })
  render(<QueriesPage database={database} />)
  return fetch
}

describe('QueriesPage coverage notice', () => {
  it('states the covered share and the breaks below 90%', async () => {
    const fetch = renderWith({ json: { database: 'shop', databases: [entry()] } })
    expect(await screen.findByTestId('coverage-notice')).toHaveTextContent(
      'Long history: 87% of execution time covered, 3 epoch breaks in 28 days')
    const urls = fetch.mock.calls.map(c => String(c[0]))
    expect(urls.some(u => u.startsWith('/api/v1/queries/coverage?database=shop'))).toBe(true)
  })

  it('says statistics reset often when most hours are breaks', async () => {
    renderWith({ json: { database: 'shop', databases: [entry({ status: 'mostly_breaks',
      covered_exec_share: 0.2, epoch_breaks: 300 })] } })
    expect(await screen.findByTestId('coverage-notice')).toHaveTextContent(
      'Statistics reset often here (compute suspends?): long-window figures are mostly breaks.')
  })

  it('says the coverage is unknown instead of calling it ok', async () => {
    renderWith({ json: { database: 'shop', databases: [entry({ status: 'unknown',
      covered_exec_share: null, hours: 0, epoch_breaks: 0 })] } })
    const notice = await screen.findByTestId('coverage-notice')
    expect(notice).toHaveTextContent('Long history: coverage unknown')
    expect(notice).not.toHaveTextContent('%')
  })

  it('marks a history under two weeks as learning', async () => {
    renderWith({ json: { database: 'shop', databases: [entry({ status: 'learning',
      covered_exec_share: 0.95, epoch_breaks: 1, hours: 240 })] } })
    expect(await screen.findByTestId('coverage-notice')).toHaveTextContent(
      'Long history: 95% of execution time covered, 1 epoch break in 10 days'
      + ' (still learning: under 14 days)')
  })

  it('is silent when ok and above 90%', async () => {
    renderWith({ json: { database: 'shop', databases: [entry({ covered_exec_share: 0.91 })] } })
    await screen.findByTestId('top-queries-empty')
    await new Promise(r => setTimeout(r, 20))
    expect(screen.queryByTestId('coverage-notice')).toBeNull()
  })

  it('is silent when the coverage cannot be read', async () => {
    renderWith({ status: 500, json: { error: 'boom' } })
    await screen.findByTestId('top-queries-empty')
    await new Promise(r => setTimeout(r, 20))
    expect(screen.queryByTestId('coverage-notice')).toBeNull()
    expect(screen.queryByText(/boom/)).toBeNull()
  })

  it('names each database in a fleet', async () => {
    renderWith({ json: { database: 'all', databases: [entry({ database: 'orders' }),
      entry({ database: 'billing', covered_exec_share: 0.99 })] } }, 'all')
    const notices = await screen.findAllByTestId('coverage-notice')
    expect(notices).toHaveLength(1)
    expect(notices[0]).toHaveTextContent('orders: Long history: 87% of execution time')
  })
})
