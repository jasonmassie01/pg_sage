import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DerivedSettings } from './DerivedSettings'

const refetch = vi.fn()
let apiState

vi.mock('../../hooks/useAPI', () => ({
  useAPI: (url) => ({ ...apiState, url, refetch }),
}))

const history = [
  {
    event: 'shadow', value: 1200, previous: 500, reason: 'catalog scan 300 ms',
    actor: 'pg_sage', at: '2026-10-04T00:00:00Z',
  },
]

function payload() {
  return {
    meaning: 'pg_sage derives these from evidence; new values soak in shadow first.',
    databases: [{
      database: 'orders',
      settings: [
        {
          key: 'safety.query_timeout_ms', unit: 'ms', class: 'derivable',
          lifecycle: 'restart', status: 'shadow', value: 500, default: 500,
          summary: 'Catalog read deadline', rule: 'catalog_read_deadline',
          rule_version: 1, note: '',
          shadow: {
            value: 1200, since: '2026-10-04T00:00:00Z', reason: '', samples: 2,
            soak_hours: 24,
          },
          pending_restart: null, pinned: null,
          evidence: [{ name: 'catalog_scan_ms', value: 300, unit: 'ms' }],
          bounds: { min: 500, max: 5000, default: 500 },
          history,
        },
        {
          key: 'collector.interval_seconds', unit: 's', class: 'derivable',
          lifecycle: 'reconfigure', status: 'operator', value: 45, default: 60,
          summary: 'Collector cadence', rule: 'collector_interval_by_self_cost',
          rule_version: 1, note: '', shadow: null, pending_restart: null,
          pinned: null, evidence: [], bounds: { min: 60, max: 600, default: 60 },
          history: [],
        },
        {
          key: 'sre.detectors.lwlock_waiters', unit: 'backends', class: 'derivable',
          lifecycle: 'restart', status: 'pinned', value: 20, default: 8,
          summary: 'LWLock contention threshold', rule: 'lwlock', rule_version: 1,
          note: '', shadow: null, pending_restart: null,
          pinned: { value: 20, by: 'admin@x.com', at: '2026-10-04T02:00:00Z' },
          evidence: [{ name: 'max_connections', value: 1000, unit: 'connections' }],
          bounds: { min: 8, max: 64, default: 8 }, history: [],
        },
        {
          key: 'sre.runways.sequence_interval_seconds', unit: 's', class: 'derivable',
          lifecycle: 'restart', status: 'derived', value: 600, default: 600,
          summary: 'Sequence sampling', rule: 'seq', rule_version: 1, note: '',
          shadow: null, pending_restart: 1500, pinned: null,
          evidence: [], bounds: { min: 600, max: 2400, default: 600 }, history: [],
        },
        {
          key: 'sre.detectors.temp_file_mb', unit: 'MiB', class: 'derivable',
          lifecycle: 'restart', status: 'default', value: 1024, default: 1024,
          summary: 'Temp-file threshold', rule: 'temp', rule_version: 1,
          note: 'temp-file rate not measured yet', shadow: null,
          pending_restart: null, pinned: null, evidence: [],
          bounds: { min: 1024, max: 65536, default: 1024 }, history: [],
        },
      ],
    }],
  }
}

beforeEach(() => {
  apiState = { data: payload(), loading: false, error: null }
  refetch.mockReset()
  globalThis.fetch = vi.fn(async () => ({
    ok: true, status: 200, json: async () => ({}),
  }))
})

afterEach(() => {
  vi.restoreAllMocks()
})

describe('DerivedSettings', () => {
  it('shows every derived key with its value, status, evidence and bounds', () => {
    render(<DerivedSettings database="orders" />)
    expect(screen.getByText('Derived settings')).toBeInTheDocument()
    const qt = screen.getByTestId('derived-safety.query_timeout_ms')
    expect(qt).toHaveTextContent('500 ms')
    expect(qt).toHaveTextContent('shadow')
    expect(qt).toHaveTextContent('1200 ms')
    expect(qt).toHaveTextContent('catalog_scan_ms')
    expect(qt).toHaveTextContent('300')
    expect(qt).toHaveTextContent('500–5000 ms')
    expect(qt).toHaveTextContent('2 samples')
    const op = screen.getByTestId('derived-collector.interval_seconds')
    expect(op).toHaveTextContent('operator')
    expect(op).toHaveTextContent('set in configuration')
    const seq = screen.getByTestId('derived-sre.runways.sequence_interval_seconds')
    expect(seq).toHaveTextContent('1500 s pending restart')
    const temp = screen.getByTestId('derived-sre.detectors.temp_file_mb')
    expect(temp).toHaveTextContent('temp-file rate not measured yet')
    const pinned = screen.getByTestId('derived-sre.detectors.lwlock_waiters')
    expect(pinned).toHaveTextContent('admin@x.com')
  })

  it('offers pin current for derived keys, unpin for pinned keys, neither for operator keys',
    () => {
      render(<DerivedSettings database="orders" />)
      expect(screen.getByTestId('pin-safety.query_timeout_ms')).toBeInTheDocument()
      expect(screen.getByTestId('unpin-sre.detectors.lwlock_waiters')).toBeInTheDocument()
      expect(screen.queryByTestId('pin-collector.interval_seconds')).toBeNull()
      expect(screen.queryByTestId('unpin-collector.interval_seconds')).toBeNull()
      expect(screen.queryByTestId('pin-sre.detectors.lwlock_waiters')).toBeNull()
    })

  it('pins the current value of the selected database and refreshes', async () => {
    render(<DerivedSettings database="orders" />)
    fireEvent.click(screen.getByTestId('pin-safety.query_timeout_ms'))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    const [url, opts] = globalThis.fetch.mock.calls[0]
    expect(url).toBe(
      '/api/v1/derived-settings/safety.query_timeout_ms/pin?database=orders')
    expect(opts.method).toBe('POST')
    await waitFor(() => expect(refetch).toHaveBeenCalled())
  })

  it('unpins and shows a server refusal', async () => {
    globalThis.fetch = vi.fn(async () => ({
      ok: false, status: 403, json: async () => ({ error: 'insufficient permissions' }),
    }))
    render(<DerivedSettings database="orders" />)
    fireEvent.click(screen.getByTestId('unpin-sre.detectors.lwlock_waiters'))
    expect(await screen.findByText(/insufficient permissions/)).toBeInTheDocument()
    expect(globalThis.fetch.mock.calls[0][0]).toBe(
      '/api/v1/derived-settings/sre.detectors.lwlock_waiters/unpin?database=orders')
    expect(refetch).not.toHaveBeenCalled()
  })

  it('shows the derivation history on demand', () => {
    render(<DerivedSettings database="orders" />)
    expect(screen.queryByText('catalog scan 300 ms')).toBeNull()
    fireEvent.click(screen.getByTestId('history-safety.query_timeout_ms'))
    expect(screen.getByText('catalog scan 300 ms')).toBeInTheDocument()
    expect(screen.getByTestId('derived-safety.query_timeout_ms'))
      .toHaveTextContent('500 → 1200')
  })

  it('omits the database parameter for all databases', () => {
    let seen
    apiState = { data: payload(), loading: false, error: null }
    const { rerender } = render(<DerivedSettings database="all" />)
    seen = screen.getByTestId('derived-settings').getAttribute('data-url')
    expect(seen).toBe('/api/v1/derived-settings')
    rerender(<DerivedSettings database="orders" />)
    seen = screen.getByTestId('derived-settings').getAttribute('data-url')
    expect(seen).toBe('/api/v1/derived-settings?database=orders')
  })

  it('renders loading, error and empty states', () => {
    apiState = { data: null, loading: true, error: null }
    const { rerender } = render(<DerivedSettings database="orders" />)
    expect(screen.queryByText('Derived settings')).toBeNull()
    apiState = { data: null, loading: false, error: '500 Internal Server Error' }
    rerender(<DerivedSettings database="orders" />)
    expect(screen.getByText(/500 Internal Server Error/)).toBeInTheDocument()
    apiState = { data: { databases: [] }, loading: false, error: null }
    rerender(<DerivedSettings database="orders" />)
    expect(screen.getByText(/No derived settings/)).toBeInTheDocument()
  })
})
