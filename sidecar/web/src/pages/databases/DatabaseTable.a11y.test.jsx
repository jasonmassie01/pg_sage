import { render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { DatabaseTable } from './DatabaseTable'
import { ReadOnlyFleet } from './ReadOnlyFleet'

// ux-12: the fleet tables' header cells are column headers to a screen
// reader (scope="col"), every header has a name (the select-all and the
// row actions column included), and the sortable ones say how they sort.
// No concurrency tests: one table, single-threaded render.
// Error propagation: not applicable, markup only.

function reg(id, name) {
  return { id, name, host: `${name}.example.com`, port: 5432, database_name: 'app',
    trust_level: 'advisory', execution_mode: 'approval', enabled: true, tags: {} }
}

const regs = [reg(1, 'orders'), reg(2, 'billing')]

function serve() {
  globalThis.fetch = vi.fn(async () => ({ ok: true, status: 200, json: async () => ({
    mode: 'fleet', summary: {}, total: 2, total_capped: false, next_cursor: '',
    databases: regs.map(r => ({ id: r.id, database_id: r.id, name: r.name, tags: [],
      status: { connected: true, health_score: 90, findings_open: 0 } })),
  }) }))
}

afterEach(() => { vi.restoreAllMocks() })

describe('DatabaseTable header cells', () => {
  it('scopes every header to its column and names each one', async () => {
    serve()
    render(<DatabaseTable databases={regs} onEdit={vi.fn()} onDelete={vi.fn()}
      onError={vi.fn()} runtimeFor={() => undefined} />)
    const table = await screen.findByTestId('databases-table')
    await waitFor(() => expect(within(table).getAllByTestId('db-row')).toHaveLength(2))
    const headers = within(table).getAllByRole('columnheader')
    expect(headers).toHaveLength(14)
    for (const th of headers) expect(th).toHaveAttribute('scope', 'col')
    for (const th of headers.slice(1)) {
      expect(th.textContent.trim().length, th.outerHTML).toBeGreaterThan(0)
    }
    expect(within(headers[0]).getByRole('checkbox', { name: 'Select all shown databases' }))
      .toBeInTheDocument()
    expect(headers.at(-1)).toHaveTextContent('Actions')
  })

  it('puts aria-sort on the sortable headers only', async () => {
    serve()
    render(<DatabaseTable databases={regs} onEdit={vi.fn()} onDelete={vi.fn()}
      onError={vi.fn()} runtimeFor={() => undefined} />)
    const table = await screen.findByTestId('databases-table')
    const th = name => within(table).getByRole('columnheader', { name: new RegExp(`^${name}`) })
    expect(th('Name')).toHaveAttribute('aria-sort', 'ascending')
    expect(th('Health')).toHaveAttribute('aria-sort', 'none')
    expect(th('Host')).not.toHaveAttribute('aria-sort')
    expect(th('Host')).toHaveAttribute('scope', 'col')
  })
})

describe('ReadOnlyFleet header cells', () => {
  it('scopes the headers with and without sorting', () => {
    const dbs = [{ name: 'orders', status: { connected: true, health_score: 80 } }]
    const { unmount } = render(<ReadOnlyFleet databases={dbs} />)
    for (const th of screen.getAllByRole('columnheader')) {
      expect(th).toHaveAttribute('scope', 'col')
    }
    unmount()
    render(<ReadOnlyFleet databases={dbs} sort={{ sort: 'name', order: 'asc' }}
      onSort={vi.fn()} />)
    for (const th of screen.getAllByRole('columnheader')) {
      expect(th).toHaveAttribute('scope', 'col')
    }
  })
})
