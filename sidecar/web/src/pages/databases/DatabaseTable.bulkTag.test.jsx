import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { DatabaseTable } from './DatabaseTable'

// ux-10a review: the bulk "Set tag" action reads each selected registration
// (GET) and writes it back with the tag added (PUT) through the per-row
// route, one at a time, then reloads the registrations. The fake server
// answers the list, the per-row GET and the PUT.
// No concurrency test: the rows are tagged one after another by design.

const SLOW = { timeout: 10000 }

function reg(id, name, tags = {}) {
  return { id, name, host: `${name}.example.com`, port: 5432, database_name: 'app',
    username: 'sage', sslmode: 'require', max_connections: 2, trust_level: 'advisory',
    execution_mode: 'approval', enabled: true, tags }
}

const regs = [reg(1, 'orders', { team: 'core' }), reg(2, 'billing'), reg(3, 'analytics')]

function fakeServer({ failPut = {} } = {}) {
  const calls = []
  globalThis.fetch = vi.fn(async (url, init = {}) => {
    const method = init.method || 'GET'
    calls.push({ url, method, body: init.body ? JSON.parse(init.body) : undefined })
    const one = url.match(/^\/api\/v1\/databases\/managed\/(\d+)$/)
    if (one && method === 'GET') {
      return { ok: true, status: 200, json: async () => regs.find(r => r.id === Number(one[1])) }
    }
    if (one && method === 'PUT') {
      if (failPut[one[1]]) {
        return { ok: false, status: 400, json: async () => ({ error: failPut[one[1]] }) }
      }
      return { ok: true, status: 200, json: async () => ({ id: Number(one[1]) }) }
    }
    const rows = [...regs].sort((a, b) => a.name.localeCompare(b.name))
      .map(r => ({ id: r.id, database_id: r.id, name: r.name, tags: [], status: {} }))
    return { ok: true, status: 200, json: async () => ({ mode: 'fleet', summary: {},
      databases: rows, total: rows.length, total_capped: false, next_cursor: '' }) }
  })
  return calls
}

function renderTable(props = {}) {
  return render(<DatabaseTable databases={regs} onEdit={vi.fn()} onDelete={vi.fn()}
    onError={vi.fn()} runtimeFor={r => ({ id: r.id, name: r.name, status: {} })} {...props} />)
}

const rowsOf = () => within(screen.getByTestId('databases-table')).getAllByTestId('db-row')
const select = name => fireEvent.click(screen.getByRole('checkbox', { name: `Select ${name}` }))
const writes = calls => calls.filter(c => c.method === 'PUT')

function setTag(value) {
  fireEvent.change(screen.getByTestId('db-bulk-tag-input'), { target: { value } })
  fireEvent.click(screen.getByTestId('db-bulk-tag'))
}

afterEach(() => { vi.restoreAllMocks() })

describe('DatabaseTable bulk tag', () => {
  it('reads then writes each selected row with the tag added, then reloads', async () => {
    const calls = fakeServer()
    const onChanged = vi.fn()
    renderTable({ onChanged })
    await waitFor(() => expect(rowsOf()).toHaveLength(3))
    select('orders')
    select('billing')
    setTag(' tier = prod ')
    expect(await screen.findByTestId('db-bulk-tag-summary', {}, SLOW))
      .toHaveTextContent('Tagged 2 with tier=prod: 2 updated, 0 failed')
    const perRow = calls.filter(c => /managed\/\d+$/.test(c.url)).map(c => `${c.method} ${c.url}`)
    expect(perRow).toEqual(['GET /api/v1/databases/managed/1', 'PUT /api/v1/databases/managed/1',
      'GET /api/v1/databases/managed/2', 'PUT /api/v1/databases/managed/2'])
    const [first, second] = writes(calls)
    expect(first.body.tags).toEqual({ team: 'core', tier: 'prod' })
    expect(first.body).toMatchObject({ name: 'orders', password: '', sslmode: 'require',
      trust_level: 'advisory', execution_mode: 'approval' })
    expect(first.body).not.toHaveProperty('action_username')
    expect(second.body.tags).toEqual({ tier: 'prod' })
    expect(onChanged).toHaveBeenCalledTimes(1)
    expect(screen.getByTestId('db-bulk-tag-input')).toHaveValue('')
  })

  it('reports a failed row and still tags the others', async () => {
    const calls = fakeServer({ failPut: { 2: 'name already exists' } })
    const onError = vi.fn()
    const onChanged = vi.fn()
    renderTable({ onError, onChanged })
    await waitFor(() => expect(rowsOf()).toHaveLength(3))
    select('analytics')
    select('billing')
    setTag('tier=prod')
    expect(await screen.findByTestId('db-bulk-tag-summary', {}, SLOW))
      .toHaveTextContent('Tagged 2 with tier=prod: 1 updated, 1 failed')
    expect(onError).toHaveBeenCalledWith('Tagging billing failed: name already exists')
    expect(writes(calls).map(c => c.url)).toEqual(['/api/v1/databases/managed/2',
      '/api/v1/databases/managed/3'])
    expect(onChanged).toHaveBeenCalledTimes(1)
  })

  it('refuses a tag that is not key=value and writes nothing', async () => {
    const calls = fakeServer()
    const onChanged = vi.fn()
    renderTable({ onChanged })
    await waitFor(() => expect(rowsOf()).toHaveLength(3))
    select('orders')
    setTag('prod')
    expect(await screen.findByTestId('db-bulk-tag-summary'))
      .toHaveTextContent('Type a tag as key=value')
    expect(calls.filter(c => /managed\/\d+$/.test(c.url))).toHaveLength(0)
    expect(onChanged).not.toHaveBeenCalled()
    expect(screen.getByTestId('db-bulk-tag-input')).toHaveValue('prod')
  })

  it('does not reload when every row failed', async () => {
    fakeServer({ failPut: { 1: 'boom' } })
    const onChanged = vi.fn()
    renderTable({ onChanged })
    await waitFor(() => expect(rowsOf()).toHaveLength(3))
    select('orders')
    setTag('tier=prod')
    expect(await screen.findByTestId('db-bulk-tag-summary', {}, SLOW))
      .toHaveTextContent('1 failed')
    expect(onChanged).not.toHaveBeenCalled()
  })

  it('offers the tag box only while rows are selected', async () => {
    fakeServer()
    renderTable()
    await waitFor(() => expect(rowsOf()).toHaveLength(3))
    expect(screen.queryByTestId('db-bulk-tag-input')).toBeNull()
    select('orders')
    expect(screen.getByTestId('db-bulk-tag-input')).toBeInTheDocument()
  })
})
