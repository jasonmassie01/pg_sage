import { render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabasesPage } from './DatabasesPage'

describe('DatabasesPage', () => {
  beforeEach(() => {
    globalThis.fetch = vi.fn(async url => {
      if (url === '/api/v1/config/global') {
        return {
          ok: true,
          json: async () => ({
            read_only: true,
            write_guidance: 'edit the YAML file',
          }),
        }
      }
      if (url.split('?')[0] === '/api/v1/databases') {
        return {
          ok: true,
          json: async () => ({
            databases: [{
              id: 7,
              name: 'orders',
              status: { trust_level: 'advisory', connected: true },
            }],
          }),
        }
      }
      if (url === '/api/v1/fleet/findings') {
        return {
          ok: true,
          json: async () => ({ findings: [], min_databases: 3, errors: [] }),
        }
      }
      if (url === '/api/v1/databases/managed') {
        throw new Error('read-only mode must not request managed routes')
      }
      throw new Error(`unexpected request: ${url}`)
    })
  })

  it('shows the live YAML fleet without advertising unavailable mutations',
    async () => {
      render(<DatabasesPage />)

      await waitFor(() => expect(
        screen.getByTestId('fleet-read-only'),
      ).toHaveTextContent(/edit the YAML file/i))
      expect(await screen.findByText('orders')).toBeInTheDocument()
      await waitFor(() => expect(
        screen.getByTestId('fleet-findings-empty'),
      ).toBeInTheDocument())
      expect(screen.queryByTestId('add-database-button'))
        .not.toBeInTheDocument()
      expect(screen.queryByTestId('import-csv-button'))
        .not.toBeInTheDocument()
    })

  it('pages the YAML fleet through the server list (ux-10a)', async () => {
    render(<DatabasesPage />)
    expect(await screen.findByTestId('read-only-fleet-table')).toBeInTheDocument()
    await waitFor(() => expect(globalThis.fetch.mock.calls.map(c => c[0]))
      .toContain('/api/v1/databases?sort=name&order=asc&limit=50'))
    expect(await screen.findByRole('searchbox', { name: 'Search databases' }))
      .toBeInTheDocument()
    expect(screen.queryAllByRole('checkbox')).toHaveLength(0)
  })
})
