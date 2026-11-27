import { render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { stubFetch } from '../test/fetchStub'
import { DatabasePage } from './DatabasePage'

// fleet-06: the Database page leads with a runtime summary card and keeps
// the raw snapshot collapsed below it. Real useAPI over a fetch double.
// No concurrent access tests: one page, single-threaded render.

const snapshotURL = '/api/v1/snapshots/latest?database=orders'
const fleetURL = '/api/v1/databases'
const lookURL = '/api/v1/fleet/lookalikes?database=orders'
const drillsPath = '/api/v1/databases/orders/restore-drills'

const orders = {
  id: 3, database_id: 3, name: 'orders',
  status: {
    connected: true, pg_version: '17.2', platform: 'cloud-sql',
    trust_level: 'advisory', health_score: 81, health_reasons: ['1 critical finding'],
    collector_last_run: '2026-11-10T12:00:00Z', analyzer_last_run: '0001-01-01T00:00:00Z',
    findings_open: 4, findings_critical: 1,
  },
  emergency_stopped: true, emergency_stopped_by: 'alice',
  emergency_stopped_at: '2026-11-10T12:30:00Z',
  llm_budget: { allocation: 1000, used: 400, weight: 1 },
}

function routes({ databases = [orders], looks = { json: { database: 'orders', lookalikes: [] } } } = {}) {
  return stubFetch({
    [snapshotURL]: { json: { snapshot: { tables: 3 } } },
    [fleetURL]: { json: { databases } },
    [lookURL]: looks,
    // drill-01d: the Recoverability panel's request (path key: any limit).
    [drillsPath]: { json: { database: 'orders', status: 'never_drilled', drills: [] } },
  })
}

afterEach(() => { vi.unstubAllGlobals() })

describe('DatabasePage runtime summary', () => {
  it('summarises version, runs, health, trust, stop and budget', async () => {
    routes()
    render(<DatabasePage database="orders" />)
    const card = await screen.findByTestId('db-summary')
    expect(await within(card).findByTestId('summary-version'))
      .toHaveTextContent('cloud-sql / 17.2')
    expect(within(card).getByTestId('summary-connection')).toHaveTextContent('connected')
    expect(within(card).getByTestId('summary-collector')).not.toHaveTextContent('never')
    expect(within(card).getByTestId('summary-analyzer')).toHaveTextContent('never')
    expect(within(card).getByTestId('summary-health')).toHaveTextContent('81')
    expect(within(card).getByTestId('summary-health')).toHaveTextContent('1 critical finding')
    expect(within(card).getByTestId('summary-trust')).toHaveTextContent('advisory')
    expect(within(card).getByTestId('summary-stop')).toHaveTextContent('stopped by alice')
    expect(within(card).getByTestId('summary-budget')).toHaveTextContent('400 / 1000')
  })

  it('keeps the raw snapshot in a collapsed section', async () => {
    routes()
    render(<DatabasePage database="orders" />)
    const raw = await screen.findByTestId('raw-snapshot')
    expect(raw.tagName).toBe('DETAILS')
    expect(raw.open).toBe(false)
    expect(within(raw).getByText(/"tables": 3/)).toBeInTheDocument()
  })

  it('says the database is not running when the runtime does not report it', async () => {
    routes({ databases: [{ ...orders, name: 'other' }] })
    render(<DatabasePage database="orders" />)
    expect(await screen.findByTestId('summary-not-running'))
      .toHaveTextContent('not running')
    expect(screen.queryByTestId('summary-budget')).toBeNull()
  })

  it('renders an absent budget as a dash', async () => {
    const { llm_budget: _omit, ...noBudget } = orders
    routes({ databases: [noBudget] })
    render(<DatabasePage database="orders" />)
    expect(await screen.findByTestId('summary-budget')).toHaveTextContent(/—/)
  })
})

describe('DatabasePage look-alikes', () => {
  it('shows a quiet note when fleet learning is not running (503)', async () => {
    routes({ looks: { status: 503, json: { error: 'fleet learning is not running' } } })
    render(<DatabasePage database="orders" />)
    expect(await screen.findByTestId('lookalikes-off'))
      .toHaveTextContent('Fleet learning is not running')
    expect(screen.queryByTestId('lookalikes-error')).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('shows an error banner, not a crash, for a malformed body', async () => {
    routes({ looks: { json: { database: 'orders', lookalikes: 'nope' } } })
    render(<DatabasePage database="orders" />)
    expect(await screen.findByTestId('lookalikes-error'))
      .toHaveTextContent('Invalid look-alikes response')
    expect(screen.getByTestId('db-summary')).toBeInTheDocument()
  })

  it('rejects a look-alike entry without a numeric similarity', async () => {
    routes({ looks: { json: { lookalikes: [{ database: 'b', similarity: 'high' }] } } })
    render(<DatabasePage database="orders" />)
    expect(await screen.findByTestId('lookalikes-error'))
      .toHaveTextContent('Invalid look-alikes response')
  })

  it('shows the server error for another failure', async () => {
    routes({ looks: { status: 500, json: { error: 'control database unreachable' } } })
    render(<DatabasePage database="orders" />)
    expect(await screen.findByTestId('lookalikes-error'))
      .toHaveTextContent('control database unreachable')
  })

  it('says there are none for zero look-alikes', async () => {
    routes()
    render(<DatabasePage database="orders" />)
    expect(await screen.findByTestId('lookalikes-none'))
      .toHaveTextContent('No look-alikes')
  })

  it('treats a null list as zero look-alikes', async () => {
    routes({ looks: { json: { database: 'orders', lookalikes: null } } })
    render(<DatabasePage database="orders" />)
    expect(await screen.findByTestId('lookalikes-none')).toBeInTheDocument()
  })

  it('lists one look-alike with its similarity', async () => {
    routes({ looks: { json: { lookalikes: [{ database: 'tenant_b', similarity: 0.92 }] } } })
    render(<DatabasePage database="orders" />)
    const list = await screen.findByTestId('lookalikes-list')
    expect(within(list).getAllByRole('listitem')).toHaveLength(1)
    expect(list).toHaveTextContent('tenant_b')
    expect(list).toHaveTextContent('92%')
  })

  it('makes no requests in fleet scope', () => {
    const fetch = routes()
    render(<DatabasePage database="all" />)
    expect(screen.getByText('Select a database from the picker above')).toBeInTheDocument()
    expect(fetch).not.toHaveBeenCalled()
  })
})
