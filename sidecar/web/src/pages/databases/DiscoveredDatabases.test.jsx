import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { DiscoveredDatabases } from './DiscoveredDatabases'

const listing = {
  discovered: [
    { id: 1, source: 'cluster', external_id: 'h:5432/orders', host: 'h', port: 5432,
      dbname: 'orders', owner: 'team-db', status: 'enrolled', database_id: 9,
      last_error: '', expires_at: '2026-11-15T10:00:00Z', already_managed: false },
    { id: 2, source: 'cluster', external_id: 'h:5432/scratch', host: 'h', port: 5432,
      dbname: 'scratch', owner: 'team-db', status: 'candidate', database_id: null,
      last_error: 'connection refused', expires_at: null, already_managed: false },
    { id: 3, source: 'rds', external_id: 'db-x', host: 'x.rds', port: 5432,
      dbname: 'app', owner: 'platform', status: 'adopted', database_id: 10,
      last_error: '', expires_at: null, already_managed: false },
  ],
  sources: [
    { source: 'cluster', last_error: '', last_count: 2 },
    { source: 'rds', last_error: 'AccessDenied: 403', last_count: 1 },
  ],
}

function mockFetch(list = listing, actionOk = true) {
  const calls = []
  globalThis.fetch = vi.fn(async (url, opts = {}) => {
    calls.push({ url, method: opts.method || 'GET', body: opts.body })
    if (url === '/api/v1/databases/discovered') {
      return { ok: true, status: 200, json: async () => list }
    }
    return {
      ok: actionOk, status: actionOk ? 200 : 400,
      json: async () => (actionOk ? { status: 'ok' } : { error: 'needs enrolled' }),
    }
  })
  return calls
}

describe('DiscoveredDatabases', () => {
  afterEach(() => { vi.restoreAllMocks() })

  it('lists discovered rows with their status, owner and source errors', async () => {
    mockFetch()
    render(<DiscoveredDatabases />)
    const table = await screen.findByTestId('discovered-table')
    expect(table).toHaveTextContent('orders')
    expect(table).toHaveTextContent('team-db')
    expect(table).toHaveTextContent('enrolled')
    expect(table).toHaveTextContent('connection refused')
    expect(screen.getByTestId('discovery-source-errors')).toHaveTextContent('rds')
    expect(screen.getByTestId('discovery-source-errors')).toHaveTextContent('403')
  })

  it('offers adopt and extend for enrolled rows and ignore for candidates only', async () => {
    mockFetch()
    render(<DiscoveredDatabases />)
    await screen.findByTestId('discovered-table')
    expect(screen.getByTestId('adopt-1')).toBeInTheDocument()
    expect(screen.getByTestId('extend-1')).toBeInTheDocument()
    expect(screen.getByTestId('ignore-2')).toBeInTheDocument()
    expect(screen.queryByTestId('adopt-2')).toBeNull()
    expect(screen.queryByTestId('ignore-1')).toBeNull()
    expect(screen.queryByTestId('adopt-3')).toBeNull()
    expect(screen.queryByTestId('ignore-3')).toBeNull()
  })

  it('posts the action and reloads the list', async () => {
    const calls = mockFetch()
    render(<DiscoveredDatabases />)
    await screen.findByTestId('discovered-table')
    fireEvent.click(screen.getByTestId('extend-1'))
    await waitFor(() => expect(calls.some(c =>
      c.url === '/api/v1/databases/discovered/1/extend' && c.method === 'POST')).toBe(true))
    const extend = calls.find(c => c.url.endsWith('/1/extend'))
    expect(JSON.parse(extend.body)).toEqual({ hours: 24 })
    fireEvent.click(screen.getByTestId('adopt-1'))
    await waitFor(() => expect(calls.filter(c =>
      c.url === '/api/v1/databases/discovered').length).toBeGreaterThanOrEqual(3))
  })

  it('shows a failed action instead of swallowing it', async () => {
    mockFetch(listing, false)
    render(<DiscoveredDatabases />)
    await screen.findByTestId('discovered-table')
    fireEvent.click(screen.getByTestId('ignore-2'))
    expect(await screen.findByRole('alert')).toHaveTextContent('needs enrolled')
  })

  it('says when nothing has been discovered', async () => {
    mockFetch({ discovered: [], sources: [] })
    render(<DiscoveredDatabases />)
    expect(await screen.findByTestId('discovered-empty')).toBeInTheDocument()
  })

  it('renders nothing when discovery is not available (404)', async () => {
    globalThis.fetch = vi.fn(async () => ({ ok: false, status: 404, json: async () => ({}) }))
    const { container } = render(<DiscoveredDatabases />)
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
    expect(await screen.findByTestId('discovery-unavailable')).toBeInTheDocument()
    expect(container.querySelector('[data-testid="discovered-table"]')).toBeNull()
  })
})
