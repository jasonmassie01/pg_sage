import { fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabaseTable } from './DatabaseTable'

// ux-08b: a database row on Fleet opens the Why not panel for that
// database ("why has nothing been proposed here?") in a row of its own.
// No concurrency test: one read per open. The table reads its own page of
// GET /api/v1/databases through the same apiGet (ux-10a), so the fake
// answers by URL.

const apiGet = vi.fn()
vi.mock('../../lib/api', async importOriginal => ({ ...(await importOriginal()),
  apiGet: (...args) => apiGet(...args) }))

const db = (id, name) => ({ id, name, host: `${name}.example.com`, port: 5432,
  database_name: 'app', trust_level: 'advisory', execution_mode: 'approval', enabled: true })

const runtime = name => ({ name, status: { connected: true, health_score: 90 } })

beforeEach(() => {
  apiGet.mockReset()
  apiGet.mockImplementation(async url => (url.startsWith('/api/v1/whynot')
    ? { database: 'billing', failing: [], links: [] }
    : { mode: 'fleet', summary: {}, databases: [runtime('billing'), runtime('orders')],
      total: 2, total_capped: false, next_cursor: '' }))
})

const whyNotCalls = () => apiGet.mock.calls.map(c => c[0])
  .filter(url => url.startsWith('/api/v1/whynot'))

describe('DatabaseTable Why not', () => {
  it('opens the panel for the row database under the row', async () => {
    render(<DatabaseTable databases={[db(1, 'orders'), db(2, 'billing')]} onEdit={vi.fn()}
      onDelete={vi.fn()} onError={vi.fn()} runtimeFor={r => runtime(r.name)} />)
    const rows = await screen.findAllByTestId('db-row')
    expect(rows).toHaveLength(2)
    expect(within(rows[0]).getByText('billing')).toBeTruthy()
    expect(whyNotCalls()).toEqual([])
    fireEvent.click(within(rows[0]).getByRole('button', { name: 'Why not?' }))
    const panel = await screen.findByTestId('why-not-panel')
    expect(whyNotCalls()).toEqual(['/api/v1/whynot?database=billing'])
    expect(rows[0].nextElementSibling.contains(panel)).toBe(true)
    expect(screen.getAllByTestId('db-row')).toHaveLength(2)
    fireEvent.click(within(rows[0]).getByRole('button', { name: 'Why not?' }))
    expect(screen.queryByTestId('why-not-panel')).toBeNull()
  })
})
