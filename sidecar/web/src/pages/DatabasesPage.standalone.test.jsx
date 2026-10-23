import { render, screen, waitFor } from '@testing-library/react'
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
    })

  it('still shows managed databases when the running list fails',
    async () => {
      mockFetch({ running: [], managed: [] })
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
    })
})
