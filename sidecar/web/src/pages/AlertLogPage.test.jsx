import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AlertLogPage } from './AlertLogPage'
import { stubFetch } from '../test/fetchStub'

// alerting-09: the Alert Log shows the statuses the writer records
// (sent, resolved, error, resolve_error, deferred), the delivery error,
// the database of each row in the fleet view, and the databases whose
// alert log could not be read.
// No concurrent access tests: one polled GET, local state only.

const row = (over = {}) => ({
  id: 1, sent_at: '2026-11-10T12:00:00Z', severity: 'critical',
  channel: 'slack', status: 'sent', error_message: '',
  category: 'slow_query', title: 'Slow query', database_name: 'a',
  ...over,
})

afterEach(() => vi.unstubAllGlobals())

async function renderLog(body, database = 'all') {
  const fetch = stubFetch({ '/api/v1/alert-log': { json: body } })
  render(<AlertLogPage database={database} />)
  await screen.findByTestId('alert-log-summary')
  return fetch
}

const statusOf = id => screen.getByTestId(`alert-status-${id}`)

describe('AlertLogPage statuses', () => {
  it('renders an error row red with its message', async () => {
    await renderLog({ alerts: [
      row({ id: 7, status: 'error', error_message: 'slack: 404 Not Found' }),
    ] })
    expect(statusOf(7)).toHaveTextContent('error')
    expect(statusOf(7).style.color).toBe('var(--red)')
    expect(screen.getByTestId('alert-error-7')).toHaveTextContent('slack: 404 Not Found')
  })

  it('renders a deferred row amber as deferred by quiet hours', async () => {
    await renderLog({ alerts: [
      row({ id: 8, status: 'deferred', channel: 'quiet_hours',
        error_message: 'deferred by quiet hours' }),
    ] })
    expect(statusOf(8)).toHaveTextContent('deferred: quiet hours')
    expect(statusOf(8).style.color).toBe('var(--yellow)')
  })

  it('renders sent and resolved green and a failed resolve red', async () => {
    await renderLog({ alerts: [
      row({ id: 1, status: 'sent' }),
      row({ id: 2, status: 'resolved' }),
      row({ id: 3, status: 'resolve_error', error_message: 'timeout' }),
    ] })
    expect(statusOf(1).style.color).toBe('var(--green)')
    expect(statusOf(2)).toHaveTextContent('resolved')
    expect(statusOf(2).style.color).toBe('var(--green)')
    expect(statusOf(3)).toHaveTextContent('resolve failed')
    expect(statusOf(3).style.color).toBe('var(--red)')
  })

  it('shows an unknown status as its raw text in the secondary colour', async () => {
    await renderLog({ alerts: [row({ id: 9, status: 'bounced' })] })
    expect(statusOf(9)).toHaveTextContent('bounced')
    expect(statusOf(9).style.color).toBe('var(--text-secondary)')
  })

  it('renders a null or empty error message as a dash', async () => {
    await renderLog({ alerts: [
      row({ id: 4, error_message: null }),
      row({ id: 5, error_message: '' }),
    ] })
    expect(screen.getByTestId('alert-error-4')).toHaveTextContent(/^-$/)
    expect(screen.getByTestId('alert-error-5')).toHaveTextContent(/^-$/)
  })

  it('shortens an error longer than 120 characters until expanded', async () => {
    const long = 'x'.repeat(119) + 'TAIL-END'
    await renderLog({ alerts: [row({ id: 6, status: 'error', error_message: long })] })
    const cell = screen.getByTestId('alert-error-6')
    expect(cell).not.toHaveTextContent('TAIL-END')
    fireEvent.click(within(cell).getByRole('button', { name: /more/i }))
    expect(cell).toHaveTextContent('TAIL-END')
  })

  it('shows an error of exactly 120 characters in full', async () => {
    const exact = 'y'.repeat(120)
    await renderLog({ alerts: [row({ id: 10, status: 'error', error_message: exact })] })
    const cell = screen.getByTestId('alert-error-10')
    expect(cell).toHaveTextContent(exact)
    expect(within(cell).queryByRole('button')).toBeNull()
  })
})

describe('AlertLogPage summary', () => {
  it('counts delivered, failed and deferred instead of a total', async () => {
    await renderLog({ alerts: [
      row({ id: 1, status: 'sent' }),
      row({ id: 2, status: 'resolved' }),
      row({ id: 3, status: 'error' }),
      row({ id: 4, status: 'resolve_error' }),
      row({ id: 5, status: 'error' }),
      row({ id: 6, status: 'deferred', channel: 'quiet_hours' }),
    ] })
    expect(screen.getByTestId('alert-count-delivered')).toHaveTextContent('2')
    expect(screen.getByTestId('alert-count-failed')).toHaveTextContent('3')
    expect(screen.getByTestId('alert-count-deferred')).toHaveTextContent('1')
    expect(screen.queryByText('Total Alerts')).toBeNull()
  })

  it('says the date range covers only the newest 100 deliveries per database', async () => {
    await renderLog({ alerts: [row()] })
    expect(screen.getByTestId('alert-log-date-note'))
      .toHaveTextContent(/newest 100 deliveries per database/i)
  })
})

describe('AlertLogPage fleet view', () => {
  it('shows a Database column only for the fleet view', async () => {
    await renderLog({ alerts: [row({ id: 1, database_name: 'orders' })] })
    expect(screen.getByText('Database')).toBeInTheDocument()
    expect(screen.getByText('orders')).toBeInTheDocument()
  })

  it('hides the Database column for one database', async () => {
    await renderLog({ alerts: [row({ id: 1, database_name: 'orders' })] }, 'orders')
    expect(screen.queryByText('Database')).toBeNull()
  })

  it('lists the databases whose alert log could not be read', async () => {
    await renderLog({ alerts: [row()], failed_databases: ['b', 'c'] })
    const banner = screen.getByTestId('alert-log-failed-databases')
    expect(banner).toHaveTextContent('b, c')
    expect(within(banner).getByRole('alert')).toHaveTextContent('b, c')
  })

  it('shows the failure banner even when no database returned rows', async () => {
    stubFetch({ '/api/v1/alert-log': { json: { alerts: [], failed_databases: ['b'] } } })
    render(<AlertLogPage database="all" />)
    expect(await screen.findByTestId('alert-log-failed-databases')).toHaveTextContent('b')
  })

  it('shows no failure banner when every database answered', async () => {
    await renderLog({ alerts: [row()], failed_databases: [] })
    expect(screen.queryByTestId('alert-log-failed-databases')).toBeNull()
  })
})

describe('AlertLogPage request', () => {
  it('encodes the database parameter', async () => {
    const fetch = await renderLog({ alerts: [row()] }, 'a&b')
    expect(fetch.mock.calls[0][0]).toBe('/api/v1/alert-log?database=a%26b')
  })

  it('sends no database parameter for the fleet view', async () => {
    const fetch = await renderLog({ alerts: [row()] })
    expect(fetch.mock.calls[0][0]).toBe('/api/v1/alert-log')
  })

  it('shows the server error when every database failed', async () => {
    stubFetch({ '/api/v1/alert-log': {
      status: 503, json: { error: 'alert log unavailable for every database' },
    } })
    render(<AlertLogPage database="all" />)
    expect(await screen.findByText(/alert log unavailable for every database/))
      .toBeInTheDocument()
  })
})
