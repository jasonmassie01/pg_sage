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
      if (url === '/api/v1/databases') {
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
      expect(screen.getByText('orders')).toBeInTheDocument()
      expect(screen.queryByTestId('add-database-button'))
        .not.toBeInTheDocument()
      expect(screen.queryByTestId('import-csv-button'))
        .not.toBeInTheDocument()
    })
})
