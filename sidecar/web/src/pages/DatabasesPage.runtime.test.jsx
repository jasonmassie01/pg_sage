import { render, screen, waitFor, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabasesPage } from './DatabasesPage'

// fleet-06: managed (UI-registered) rows show the runtime state the
// sidecar reports for them, so a disconnected, stopped or budget-exhausted
// database never reads "enabled". No concurrent access tests: one page,
// single-threaded render.

const managedOrders = {
  id: 3, name: 'orders', host: 'db', port: 5432, database_name: 'orders',
  trust_level: 'observation', execution_mode: 'approval', enabled: true,
}
const managedBilling = {
  id: 4, name: 'billing', host: 'db', port: 5432, database_name: 'billing',
  trust_level: 'advisory', execution_mode: 'manual', enabled: true,
}
const runtimeOrders = {
  id: 3, database_id: 3, name: 'orders',
  status: {
    connected: false, health_score: 42, health_reasons: ['collector stale 42m'],
    trust_level: 'advisory', findings_open: 5, findings_critical: 2,
    platform: 'cloud-sql', pg_version: '17.2',
  },
  emergency_stopped: true, emergency_stopped_by: 'alice',
  emergency_stopped_at: '2026-11-10T12:00:00Z',
  llm_budget: { allocation: 1000, used: 250, weight: 1 },
}

function mockFetch({ running = [], managed = [], leader, runtimeFails = false }) {
  globalThis.fetch = vi.fn(async url => {
    if (url === '/api/v1/config/global') {
      return { ok: true, json: async () => ({ read_only: false }) }
    }
    if (url.split('?')[0] === '/api/v1/databases') {
      if (runtimeFails) return { ok: false, status: 502, json: async () => ({}) }
      return { ok: true, json: async () => ({ databases: running }) }
    }
    if (url === '/api/v1/databases/managed') {
      return { ok: true, json: async () => ({ databases: managed }) }
    }
    if (url === '/api/v1/fleet/findings') {
      return { ok: true, json: async () => ({ findings: [], min_databases: 3, errors: [] }) }
    }
    if (url === '/api/v1/fleet/leader') {
      if (!leader) return { ok: false, status: 404, json: async () => ({}) }
      return { ok: true, json: async () => leader }
    }
    throw new Error(`unexpected request: ${url}`)
  })
}

async function managedRow(name) {
  await waitFor(() => expect(screen.getByTestId('databases-table')).toBeInTheDocument())
  const row = screen.getAllByTestId('db-row')
    .find(r => within(r).queryByText(name, { exact: true }))
  if (!row) throw new Error(`no managed row named ${name}`)
  return row
}

describe('DatabasesPage managed rows with runtime state', () => {
  beforeEach(() => { vi.restoreAllMocks() })

  it('shows disconnection, health, findings, stop actor and budget', async () => {
    mockFetch({ running: [runtimeOrders], managed: [managedOrders] })
    render(<DatabasesPage />)
    const row = await managedRow('orders')
    expect(within(row).getByTestId('runtime-connection')).toHaveTextContent('disconnected')
    expect(within(row).getByTestId('runtime-health')).toHaveTextContent('42')
    expect(within(row).getByTestId('runtime-health')).toHaveTextContent('collector stale 42m')
    expect(within(row).getByTestId('runtime-findings')).toHaveTextContent('5 open · 2 critical')
    expect(within(row).getByTestId('runtime-stop')).toHaveTextContent('stopped by alice')
    expect(within(row).getByTestId('runtime-budget')).toHaveTextContent('250 / 1000')
    expect(row).not.toHaveTextContent('enabled')
  })

  it('marks an effective trust that differs from the registration', async () => {
    mockFetch({ running: [runtimeOrders], managed: [managedOrders] })
    render(<DatabasesPage />)
    const row = await managedRow('orders')
    expect(within(row).getByTestId('runtime-trust')).toHaveTextContent('advisory')
    expect(within(row).getByTestId('runtime-trust-override')).toHaveTextContent('override')
  })

  it('shows no override mark when the stored trust equals the registered one', async () => {
    mockFetch({
      running: [{ ...runtimeOrders, status: { ...runtimeOrders.status, trust_level: 'observation' } }],
      managed: [managedOrders],
    })
    render(<DatabasesPage />)
    const row = await managedRow('orders')
    expect(within(row).getByTestId('runtime-trust')).toHaveTextContent('observation')
    expect(within(row).queryByTestId('runtime-trust-override')).toBeNull()
  })

  it('says "not running" for a managed record the runtime does not report', async () => {
    mockFetch({ running: [runtimeOrders], managed: [managedOrders, managedBilling] })
    render(<DatabasesPage />)
    const row = await managedRow('billing')
    expect(within(row).getByTestId('runtime-connection')).toHaveTextContent('not running')
    expect(row).not.toHaveTextContent('enabled')
    expect(within(row).getByTestId('runtime-budget')).toHaveTextContent('—')
    expect(within(row).queryByTestId('runtime-inspect')).toBeNull()
  })

  it('renders an absent llm_budget as a dash', async () => {
    const { llm_budget: _omit, ...noBudget } = runtimeOrders
    mockFetch({ running: [noBudget], managed: [managedOrders] })
    render(<DatabasesPage />)
    const row = await managedRow('orders')
    expect(within(row).getByTestId('runtime-budget')).toHaveTextContent(/^—$/)
  })

  it('flags an exhausted budget', async () => {
    mockFetch({
      running: [{ ...runtimeOrders, llm_budget: { allocation: 100, used: 100, weight: 1 } }],
      managed: [managedOrders],
    })
    render(<DatabasesPage />)
    const row = await managedRow('orders')
    expect(within(row).getByTestId('runtime-budget')).toHaveTextContent('100 / 100')
    expect(within(row).getByTestId('runtime-budget')).toHaveTextContent('exhausted')
  })

  it('joins on database_id so a renamed registration keeps its runtime row', async () => {
    mockFetch({
      running: [{ ...runtimeOrders, name: 'orders-old' }],
      managed: [managedOrders],
    })
    render(<DatabasesPage />)
    const row = await managedRow('orders')
    expect(within(row).getByTestId('runtime-connection')).toHaveTextContent('disconnected')
    // The joined runtime row is not repeated as a config-file database.
    expect(screen.queryByTestId('config-file-databases')).toBeNull()
  })

  it('does not join a same-named runtime row of another database id', async () => {
    mockFetch({
      running: [{ ...runtimeOrders, id: 9, database_id: 9 }],
      managed: [managedOrders],
    })
    render(<DatabasesPage />)
    const row = await managedRow('orders')
    expect(within(row).getByTestId('runtime-connection')).toHaveTextContent('not running')
  })

  it('joins on name when the runtime row has no database id', async () => {
    mockFetch({
      running: [{ ...runtimeOrders, id: 0, database_id: 0 }],
      managed: [managedOrders],
    })
    render(<DatabasesPage />)
    const row = await managedRow('orders')
    expect(within(row).getByTestId('runtime-connection')).toHaveTextContent('disconnected')
  })

  it('says "unknown", not "not running", when the runtime inventory failed', async () => {
    mockFetch({ managed: [managedOrders], runtimeFails: true })
    render(<DatabasesPage />)
    const row = await managedRow('orders')
    expect(within(row).getByTestId('runtime-connection')).toHaveTextContent('unknown')
    expect(row).not.toHaveTextContent('not running')
  })

  it('links a running managed database to its inspection page', async () => {
    mockFetch({ running: [runtimeOrders], managed: [managedOrders] })
    const select = vi.fn()
    render(<DatabasesPage onSelectDatabase={select} />)
    const row = await managedRow('orders')
    const link = within(row).getByTestId('runtime-inspect')
    expect(link).toHaveAttribute('href', '#/database')
    link.click()
    expect(select).toHaveBeenCalledWith('orders')
  })

  it('keeps the Test, Edit and Delete controls on managed rows', async () => {
    mockFetch({ running: [runtimeOrders], managed: [managedOrders] })
    render(<DatabasesPage />)
    const row = await managedRow('orders')
    expect(within(row).getByTestId('db-test-button')).toBeInTheDocument()
    expect(within(row).getByTestId('db-edit-button')).toBeInTheDocument()
    expect(within(row).getByTestId('db-delete-button')).toBeInTheDocument()
  })
})

describe('DatabasesPage leader badge', () => {
  beforeEach(() => { vi.restoreAllMocks() })

  it('says "leader" when this sidecar holds the lease', async () => {
    mockFetch({ managed: [managedOrders], leader: { enabled: true, leader: true } })
    render(<DatabasesPage />)
    expect(await screen.findByTestId('fleet-leader-badge')).toHaveTextContent(/^leader$/)
    const calls = globalThis.fetch.mock.calls.filter(([url]) => url === '/api/v1/fleet/leader')
    expect(calls).toHaveLength(1)
  })

  it('names the holder when this sidecar follows', async () => {
    mockFetch({
      managed: [managedOrders],
      leader: { enabled: true, leader: false, holder: 'sage-b' },
    })
    render(<DatabasesPage />)
    expect(await screen.findByTestId('fleet-leader-badge'))
      .toHaveTextContent('follower of sage-b')
  })

  it('shows no badge when leader election is disabled', async () => {
    mockFetch({ managed: [managedOrders], leader: { enabled: false } })
    render(<DatabasesPage />)
    await managedRow('orders')
    await waitFor(() => expect(globalThis.fetch.mock.calls.map(([u]) => u))
      .toContain('/api/v1/fleet/leader'))
    expect(screen.queryByTestId('fleet-leader-badge')).toBeNull()
  })

  it('shows no badge and no alert when the leader route fails', async () => {
    mockFetch({ managed: [managedOrders] })
    render(<DatabasesPage />)
    await managedRow('orders')
    expect(screen.queryByTestId('fleet-leader-badge')).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
  })
})

describe('DatabasesPage read-only fleet runtime cells', () => {
  beforeEach(() => { vi.restoreAllMocks() })

  it('shows the same runtime state on a YAML fleet row', async () => {
    globalThis.fetch = vi.fn(async url => {
      if (url === '/api/v1/config/global') {
        return { ok: true, json: async () => ({ read_only: true }) }
      }
      if (url.split('?')[0] === '/api/v1/databases') {
        return { ok: true, json: async () => ({ databases: [runtimeOrders] }) }
      }
      if (url === '/api/v1/fleet/findings') {
        return { ok: true, json: async () => ({ findings: [], errors: [] }) }
      }
      throw new Error(`unexpected request: ${url}`)
    })
    render(<DatabasesPage />)
    const list = await screen.findByTestId('yaml-fleet-list')
    expect(within(list).getByTestId('runtime-connection')).toHaveTextContent('disconnected')
    expect(within(list).getByTestId('runtime-stop')).toHaveTextContent('stopped by alice')
    expect(within(list).getByTestId('runtime-budget')).toHaveTextContent('250 / 1000')
    // No registration to compare against: never an override mark.
    expect(within(list).queryByTestId('runtime-trust-override')).toBeNull()
  })
})
