import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabasesPage } from './DatabasesPage'

// Standalone mode: the monitored database comes from the YAML file's
// postgres section, not from the managed (UI-registered) list. The page
// must show it, read-only, next to any databases added in the UI.

function mockFetch({ running, managed }) {
  globalThis.fetch = vi.fn(async url => {
    if (url === '/api/v1/config/global') {
      return { ok: true, json: async () => ({ mode: 'standalone' }) }
    }
    if (url === '/api/v1/databases') {
      return { ok: true, json: async () => ({ databases: running }) }
    }
    if (url === '/api/v1/databases/managed') {
      return { ok: true, json: async () => ({ databases: managed }) }
    }
    if (url === '/api/v1/fleet/findings') {
      return {
        ok: true,
        json: async () => ({ findings: [], min_databases: 3, errors: [] }),
      }
    }
    throw new Error(`unexpected request: ${url}`)
  })
}

const lifeos = {
  id: 0, database_id: 0, name: 'lifeos',
  status: { trust_level: 'autonomous', connected: true },
}

describe('DatabasesPage in standalone mode', () => {
  beforeEach(() => { vi.restoreAllMocks() })

  it('lists the database from the config file', async () => {
    mockFetch({ running: [lifeos], managed: [] })
    render(<DatabasesPage />)

    await waitFor(() => expect(
      screen.getByTestId('config-file-databases'),
    ).toHaveTextContent('lifeos'))
    expect(screen.getByTestId('config-file-databases'))
      .toHaveTextContent(/autonomous/)
    expect(screen.getByTestId('config-file-note'))
      .toHaveTextContent(/config file/i)
    // Adding databases in the UI still works next to it.
    expect(screen.getByTestId('add-database-button')).toBeInTheDocument()
    expect(screen.queryByText(/No databases configured/)).toBeNull()
    expect(screen.getByTestId('fleet-inventory-summary')).toHaveTextContent('Known databases1')
    expect(screen.getByRole('link', { name: 'Inspect database' })).toHaveAttribute('href', '#/database')
  })

  it('shows a UI-added database only once, as editable', async () => {
    const orders = { id: 3, name: 'orders', host: 'db', port: 5432,
      database_name: 'orders', enabled: true }
    mockFetch({
      running: [lifeos, { id: 3, database_id: 3, name: 'orders',
        status: { connected: true } }],
      managed: [orders],
    })
    render(<DatabasesPage />)

    await waitFor(() => expect(
      screen.getByTestId('config-file-databases'),
    ).toHaveTextContent('lifeos'))
    expect(screen.getByTestId('config-file-databases'))
      .not.toHaveTextContent('orders')
    expect(screen.getAllByText('orders')).toHaveLength(1)
  })

  it('omits the config-file section when every database is managed',
    async () => {
      mockFetch({ running: [], managed: [] })
      render(<DatabasesPage />)

      await waitFor(() => expect(
        screen.getByTestId('add-database-button'),
      ).toBeInTheDocument())
      expect(screen.queryByTestId('config-file-databases'))
        .not.toBeInTheDocument()
      expect(screen.getByTestId('fleet-inventory-empty')).toBeInTheDocument()
    })

  it('still shows managed databases when the running list fails',
    async () => {
      mockFetch({ running: [], managed: [{ id: 3, name: 'orders', enabled: true }] })
      const base = globalThis.fetch
      globalThis.fetch = vi.fn(async url => {
        if (url === '/api/v1/databases') return { ok: false, json: async () => ({}) }
        return base(url)
      })
      render(<DatabasesPage />)

      await waitFor(() => expect(
        screen.getByTestId('add-database-button'),
      ).toBeInTheDocument())
      expect(screen.queryByTestId('config-file-databases'))
        .not.toBeInTheDocument()
      expect(screen.getByText('orders')).toBeInTheDocument()
      expect(screen.getByRole('alert')).toHaveTextContent('Failed to load runtime database inventory')
      expect(screen.getByTestId('fleet-inventory-summary')).toHaveTextContent('Runtime connectionsUnavailable')
      expect(screen.queryByTestId('fleet-inventory-empty')).toBeNull()
    })

  it('keeps runtime databases visible when managed registration fails and recovers on retry', async () => {
    mockFetch({ running: [lifeos], managed: [] })
    const base = globalThis.fetch
    let failManaged = true
    globalThis.fetch = vi.fn(async url => {
      if (url === '/api/v1/databases/managed' && failManaged) return { ok: false, status: 503 }
      return base(url)
    })
    render(<DatabasesPage />)
    expect(await screen.findByText('lifeos')).toBeInTheDocument()
    expect(screen.getByRole('alert')).toHaveTextContent('managed database registrations (HTTP 503)')
    expect(screen.getByTestId('fleet-read-only')).toHaveTextContent('could not be verified')
    expect(screen.queryByTestId('add-database-button')).toBeNull()
    expect(screen.queryByText(/No databases configured/)).toBeNull()
    failManaged = false
    fireEvent.click(screen.getByRole('button', { name: 'Retry inventory' }))
    expect(await screen.findByTestId('add-database-button')).toBeInTheDocument()
    expect(screen.getByTestId('config-file-databases')).toHaveTextContent('lifeos')
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('does not report unknown connectivity as disconnected', async () => {
    mockFetch({ running: [{ name: 'starting' }], managed: [] })
    render(<DatabasesPage />)
    const inventory = await screen.findByTestId('config-file-databases')
    expect(inventory).toHaveTextContent('unknown')
    expect(inventory).not.toHaveTextContent('disconnected')
  })

  it('selects the inspected runtime database before navigating', async () => {
    mockFetch({ running: [lifeos], managed: [] })
    const select = vi.fn()
    render(<DatabasesPage onSelectDatabase={select} />)
    fireEvent.click(await screen.findByRole('link', { name: 'Inspect database' }))
    expect(select).toHaveBeenCalledWith('lifeos')
  })

  it('keeps live inventory read-only when configuration fails', async () => {
    mockFetch({ running: [lifeos], managed: [] })
    const base = globalThis.fetch
    globalThis.fetch = vi.fn(async url => {
      if (url === '/api/v1/config/global') throw new Error('configuration offline')
      return base(url)
    })
    render(<DatabasesPage />)
    expect(await screen.findByText('lifeos')).toBeInTheDocument()
    expect(screen.queryByTestId('add-database-button')).toBeNull()
    expect(screen.getByRole('alert')).toHaveTextContent('configuration offline')
    expect(globalThis.fetch.mock.calls.map(([url]) => url)).not.toContain('/api/v1/databases/managed')
  })

  it('accepts null empty lists without mistaking a malformed response for an empty fleet', async () => {
    mockFetch({ running: null, managed: null })
    const base = globalThis.fetch
    let malformed = true
    globalThis.fetch = vi.fn(async url => {
      if (url === '/api/v1/databases' && malformed) return { ok: true, json: async () => ({}) }
      return base(url)
    })
    render(<DatabasesPage />)
    expect(await screen.findByRole('alert')).toHaveTextContent('Invalid runtime database inventory response')
    expect(screen.queryByTestId('fleet-inventory-empty')).toBeNull()
    malformed = false
    fireEvent.click(screen.getByRole('button', { name: 'Retry inventory' }))
    expect(await screen.findByTestId('fleet-inventory-empty')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).toBeNull()
  })
})
