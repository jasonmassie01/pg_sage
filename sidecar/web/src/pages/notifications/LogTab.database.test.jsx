import { render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { LogTab } from './LogTab'
import { stubFetch } from '../../test/fetchStub'

// alerting-06c: the Log tab filters by the selected database and names the
// database of each delivery; a delivery of no database reads "unknown".
// No concurrent access tests: one fetch per render, local state only.

afterEach(() => vi.unstubAllGlobals())

const row = (over = {}) => ({
  id: 1, event: 'finding_critical', subject: 'Slow query', status: 'sent', error: '',
  sent_at: '2026-11-10T12:00:00Z', database: 'orders', dedup_key: 'finding:1', ...over,
})

describe('LogTab database filter', () => {
  it('asks the API for the selected database only', async () => {
    const fetch = stubFetch({ '/api/v1/notifications/log': { json: { log: [row()] } } })
    render(<LogTab database="orders" />)
    expect(await screen.findByText('Slow query')).toBeInTheDocument()
    expect(fetch.mock.calls[0][0]).toBe('/api/v1/notifications/log?limit=100&database=orders')
  })

  it('sends no filter for the fleet-wide scope or no scope', async () => {
    for (const database of ['all', undefined, '']) {
      const fetch = stubFetch({ '/api/v1/notifications/log': { json: { log: [] } } })
      const { unmount } = render(<LogTab database={database} />)
      expect(await screen.findByText('No notifications sent yet.')).toBeInTheDocument()
      expect(fetch.mock.calls[0][0]).toBe('/api/v1/notifications/log?limit=100')
      unmount()
    }
  })

  it('encodes the database name in the query', async () => {
    const fetch = stubFetch({ '/api/v1/notifications/log': { json: { log: [] } } })
    render(<LogTab database="a&b" />)
    await screen.findByText('No notifications sent yet.')
    expect(fetch.mock.calls[0][0]).toBe('/api/v1/notifications/log?limit=100&database=a%26b')
  })

  it('shows a Database column, with unknown for a delivery of no database', async () => {
    stubFetch({ '/api/v1/notifications/log': { json: { log: [
      row(), row({ id: 2, subject: 'Broadcast', database: 'unknown' }),
      row({ id: 3, subject: 'Legacy', database: undefined }),
    ] } } })
    render(<LogTab />)
    const table = await screen.findByRole('table')
    expect(within(table).getByText('Database')).toBeInTheDocument()
    expect(screen.getByTestId('notify-database-1')).toHaveTextContent('orders')
    expect(screen.getByTestId('notify-database-2')).toHaveTextContent('unknown')
    expect(screen.getByTestId('notify-database-3')).toHaveTextContent('unknown')
    expect(table).not.toHaveTextContent(/\ball\b/)
  })
})
