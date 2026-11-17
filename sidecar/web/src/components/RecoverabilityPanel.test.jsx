import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { FleetRecoverabilityLine, RecoverabilityPanel } from './RecoverabilityPanel'
import { formatDuration } from '../lib/recoverability'

// drill-01d: the database page's Recoverability panel and the fleet
// overview's one-line count. Payload shapes follow the Go route's golden
// files (internal/api/testdata/restore_drills_golden.json and
// fleet_recoverability_golden.json). No concurrency tests: the panel is
// a render of one query; the API test runs concurrent requests.

let queries = {}
let requested = []
vi.mock('../hooks/useAPI', () => ({
  useAPI: (url) => {
    requested.push(url)
    return queries[url] || { data: null, loading: true, error: null, refetch: vi.fn() }
  },
}))

const historyURL = '/api/v1/databases/orders/restore-drills?limit=20'
const fleetURL = '/api/v1/fleet/recoverability'

function ok(data) {
  return { data, loading: false, error: null, refetch: vi.fn() }
}

const pass = {
  id: 2, database: 'orders', provider: 'rds_restore', kind: 'full_copy',
  requested_at: '2026-11-15T11:49:00Z', finished_at: '2026-11-15T12:00:00Z',
  recovery_point: '2026-11-15T11:45:00Z', rto_seconds: 660, rpo_seconds: 240,
  invariants: [], passed: true, cost_label: 'about $1.40 (estimate)',
}

const fail = {
  id: 1, database: 'orders', provider: 'rds_restore', kind: 'full_copy',
  requested_at: '2026-11-07T11:51:00Z', finished_at: '2026-11-07T12:00:00Z',
  invariants: [], passed: false, failure: 'row_count app.orders: 90 rows, source 100',
}

function history(over = {}) {
  return {
    database: 'orders', status: 'passed', window_days: 35, latest: pass,
    last_pass: pass, drills: [pass, fail], isolation: null,
    next_full_copy_cost_label: 'about $1.40 (estimate)', ...over,
  }
}

describe('RecoverabilityPanel', () => {
  beforeEach(() => {
    queries = {}
    requested = []
  })

  it('shows a passed drill with RPO and RTO in plain units', () => {
    queries[historyURL] = ok(history())
    render(<RecoverabilityPanel database="orders" />)
    expect(requested).toContain(historyURL)
    expect(screen.getByRole('heading', { name: 'Recoverability' })).toBeInTheDocument()
    expect(screen.getByTestId('recoverability-status')).toHaveTextContent('Passed')
    expect(screen.getByTestId('recoverability-rpo')).toHaveTextContent('RPO 4 min')
    expect(screen.getByTestId('recoverability-rto')).toHaveTextContent('RTO 11 min')
    expect(screen.getByTestId('recoverability-cost'))
      .toHaveTextContent('Next full-copy drill: about $1.40 (estimate)')
    expect(screen.queryByTestId('recoverability-failure')).toBeNull()
  })

  it('says Never drilled with the reason when there are no drills', () => {
    queries[historyURL] = ok(history({
      status: 'never_drilled', latest: undefined, last_pass: undefined, drills: [],
      next_full_copy_cost_label: undefined,
      reason: 'no restore drill has run yet',
    }))
    render(<RecoverabilityPanel database="orders" />)
    expect(screen.getByTestId('recoverability-status')).toHaveTextContent('Never drilled')
    expect(screen.getByText(/no restore drill has run yet/)).toBeInTheDocument()
    expect(screen.queryByTestId('recoverability-rpo')).toBeNull()
  })

  it('renders an unknown RPO as unknown with its reason', () => {
    const unmarked = { ...pass, rpo_seconds: undefined, rpo_reason: 'no sage.snapshots' }
    queries[historyURL] = ok(history({ latest: unmarked, last_pass: unmarked,
      drills: [unmarked] }))
    render(<RecoverabilityPanel database="orders" />)
    expect(screen.getByTestId('recoverability-rpo')).toHaveTextContent('RPO unknown')
    expect(screen.getByTestId('recoverability-rpo')).toHaveTextContent('no sage.snapshots')
  })

  it('says what to configure when recoverability is unknown', () => {
    queries[historyURL] = ok(history({
      status: 'unknown', latest: undefined, last_pass: undefined, drills: [],
      reason: 'clone.provider is none', advice: 'Set clone.provider and grant it access.',
    }))
    render(<RecoverabilityPanel database="orders" />)
    expect(screen.getByTestId('recoverability-status')).toHaveTextContent('Unknown')
    expect(screen.getByTestId('recoverability-unknown'))
      .toHaveTextContent('Unknown: clone.provider is none. Set clone.provider and grant')
  })

  it('shows the shared QueryState error on an API 500, not an empty panel', () => {
    queries[historyURL] = { data: null, loading: false, error: 'restore drills of orders ' +
      'could not be read', errorStatus: 500, refetch: vi.fn() }
    render(<RecoverabilityPanel database="orders" />)
    expect(screen.getByRole('alert'))
      .toHaveTextContent('restore drills of orders could not be read')
    expect(screen.queryByTestId('recoverability-status')).toBeNull()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument()
  })

  it('drops the failure banner when a failed database passes on rerender', () => {
    queries[historyURL] = ok(history({ status: 'failed', latest: fail, last_pass: undefined,
      drills: [fail], reason: fail.failure }))
    const { rerender } = render(<RecoverabilityPanel database="orders" />)
    expect(screen.getByTestId('recoverability-failure'))
      .toHaveTextContent('row_count app.orders: 90 rows, source 100')
    expect(screen.getByTestId('recoverability-status')).toHaveTextContent('Failed')
    queries[historyURL] = ok(history())
    rerender(<RecoverabilityPanel database="orders" />)
    expect(screen.queryByTestId('recoverability-failure')).toBeNull()
    expect(screen.getByTestId('recoverability-status')).toHaveTextContent('Passed')
  })

  it('shows whether backups survive a delete, and unknown when not read', () => {
    queries[historyURL] = ok(history({ isolation: { survives_delete: false,
      evidence: 'only automated backups' } }))
    const { rerender } = render(<RecoverabilityPanel database="orders" />)
    expect(screen.getByTestId('recoverability-isolation'))
      .toHaveTextContent('Backups are deleted with the database: only automated backups')
    queries[historyURL] = ok(history({ isolation: { survives_delete: true,
      evidence: 'a manual snapshot 3 h old' } }))
    rerender(<RecoverabilityPanel database="orders" />)
    expect(screen.getByTestId('recoverability-isolation'))
      .toHaveTextContent('Backups survive a delete of the database: a manual snapshot 3 h old')
    queries[historyURL] = ok(history({ isolation: null }))
    rerender(<RecoverabilityPanel database="orders" />)
    expect(screen.getByTestId('recoverability-isolation'))
      .toHaveTextContent('Unknown whether backups survive a delete')
  })

  it('requests nothing without one database', () => {
    render(<RecoverabilityPanel database="all" />)
    expect(requested.every(u => u === null)).toBe(true)
    expect(screen.queryByTestId('recoverability')).toBeNull()
  })
})

describe('FleetRecoverabilityLine', () => {
  beforeEach(() => {
    queries = {}
    requested = []
  })

  it('reads 7 of 9 databases passed a restore drill in the last 35 days', () => {
    queries[fleetURL] = ok({ window_days: 35, total: 9, passed: 7, failed: 1, stale: 0,
      never_drilled: 0, unknown: 1, median_rto_seconds: 660, databases: [] })
    render(<FleetRecoverabilityLine />)
    expect(screen.getByTestId('fleet-recoverability'))
      .toHaveTextContent('7 of 9 databases passed a restore drill in the last 35 days')
    expect(screen.getByTestId('fleet-recoverability')).toHaveTextContent('median RTO 11 min')
  })

  it('uses the singular for one database and omits an unknown median', () => {
    queries[fleetURL] = ok({ window_days: 35, total: 1, passed: 0, failed: 0, stale: 0,
      never_drilled: 1, unknown: 0, median_rto_seconds: null, databases: [] })
    render(<FleetRecoverabilityLine />)
    const line = screen.getByTestId('fleet-recoverability')
    expect(line).toHaveTextContent('0 of 1 database passed a restore drill in the last 35 days')
    expect(line).not.toHaveTextContent('median')
  })

  it('renders nothing while loading, on error or for a malformed payload', () => {
    const { rerender } = render(<FleetRecoverabilityLine />)
    expect(screen.queryByTestId('fleet-recoverability')).toBeNull()
    queries[fleetURL] = { data: null, loading: false, error: 'boom', refetch: vi.fn() }
    rerender(<FleetRecoverabilityLine />)
    expect(screen.queryByTestId('fleet-recoverability')).toBeNull()
    queries[fleetURL] = ok({ databases: [] })
    rerender(<FleetRecoverabilityLine />)
    expect(screen.queryByTestId('fleet-recoverability')).toBeNull()
  })
})

describe('formatDuration', () => {
  it('renders seconds, minutes and hours in plain units', () => {
    expect(formatDuration(240)).toBe('4 min')
    expect(formatDuration(660)).toBe('11 min')
    expect(formatDuration(45)).toBe('45 s')
    expect(formatDuration(0)).toBe('0 s')
    expect(formatDuration(5400)).toBe('1 h 30 min')
    expect(formatDuration(7200)).toBe('2 h')
    expect(formatDuration(null)).toBe('unknown')
    expect(formatDuration(undefined)).toBe('unknown')
    expect(formatDuration(-5)).toBe('unknown')
    expect(formatDuration('abc')).toBe('unknown')
  })
})
