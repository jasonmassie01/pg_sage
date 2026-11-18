import { fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabaseTable } from './DatabaseTable'

// ux-08b: a database row on Fleet opens the Why not panel for that
// database ("why has nothing been proposed here?") in a row of its own.
// No concurrency test: one read per open.

const apiGet = vi.fn()
vi.mock('../../lib/api', async importOriginal => ({ ...(await importOriginal()),
  apiGet: (...args) => apiGet(...args) }))

const db = (id, name) => ({ id, name, host: `${name}.example.com`, port: 5432,
  database_name: 'app', trust_level: 'advisory', execution_mode: 'approval', enabled: true })

beforeEach(() => {
  apiGet.mockReset()
  apiGet.mockResolvedValue({ database: 'billing', failing: [], links: [] })
})

describe('DatabaseTable Why not', () => {
  it('opens the panel for the row database under the row', async () => {
    render(<DatabaseTable databases={[db(1, 'orders'), db(2, 'billing')]} onEdit={vi.fn()}
      onDelete={vi.fn()} onError={vi.fn()} />)
    const rows = screen.getAllByTestId('db-row')
    expect(rows).toHaveLength(2)
    fireEvent.click(within(rows[1]).getByRole('button', { name: 'Why not?' }))
    const panel = await screen.findByTestId('why-not-panel')
    expect(apiGet.mock.calls[0][0]).toBe('/api/v1/whynot?database=billing')
    expect(rows[1].nextElementSibling.contains(panel)).toBe(true)
    expect(screen.getAllByTestId('db-row')).toHaveLength(2)
    fireEvent.click(within(rows[1]).getByRole('button', { name: 'Why not?' }))
    expect(screen.queryByTestId('why-not-panel')).toBeNull()
  })
})
