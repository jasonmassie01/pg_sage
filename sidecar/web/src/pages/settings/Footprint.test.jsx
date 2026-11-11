import { fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Footprint } from './Footprint'

// footprint-08: the Footprint panel shows what pg_sage costs each database
// and which component spends it; a value not measured yet says so instead
// of showing 0.

let apiState

vi.mock('../../hooks/useAPI', () => ({
  useAPI: (url) => ({ ...apiState, url, refetch: vi.fn() }),
}))

function unknownDatabase(name) {
  return {
    database: name, known: { window: false, statements: false, storage: false },
    window_seconds: null, cycle_seconds: null, db_time_ms_per_hour: null,
    db_time_budget_ms_per_hour: null, db_time_budget_source: 'none',
    calls_per_hour: null, blocks_per_hour: null, blocks_budget_per_hour: null,
    rows_read_per_hour: null, rows_written_per_hour: null, schema_bytes: null,
    storage_budget_bytes: null, unattributed_share: null,
    top_statements: [], by_component: [],
  }
}

function measuredDatabase(name) {
  return {
    ...unknownDatabase(name),
    known: { window: true, statements: true, storage: true },
    window_seconds: 600, cycle_seconds: 60, db_time_ms_per_hour: 2400,
    db_time_budget_ms_per_hour: 180000,
    db_time_budget_source: 'analyzer.self_cost_budget_ms',
    calls_per_hour: 90, blocks_per_hour: 360, blocks_budget_per_hour: 18000000,
    rows_read_per_hour: 3600, rows_written_per_hour: 360, schema_bytes: 8388608,
    storage_budget_bytes: 10737418240, unattributed_share: 0.1,
    top_statements: [{ text: 'SELECT count(*) FROM pg_class', component: 'collector',
      db_time_ms: 300, calls: 10, blocks: 50 }],
    by_component: [
      { component: 'collector', db_time_ms_per_hour: 1800, calls_per_hour: 60,
        blocks_per_hour: 300, share: 0.75 },
      { component: 'sre:probe', db_time_ms_per_hour: 600, calls_per_hour: 30,
        blocks_per_hour: 60, share: 0.25 },
    ],
  }
}

function payload(databases, process) {
  return {
    meaning: 'What pg_sage itself costs each database it monitors.',
    databases,
    process: process ?? { known: false, cpu_ms_per_cycle: null,
      cpu_budget_ms_per_cycle: 600, top_loops: [] },
  }
}

beforeEach(() => {
  apiState = { data: payload([measuredDatabase('orders')]), loading: false, error: null }
})

describe('Footprint', () => {
  it('renders not measured yet for every null figure', () => {
    apiState = { data: payload([unknownDatabase('orders')]), loading: false, error: null }
    render(<Footprint database="orders" />)
    const row = screen.getByTestId('footprint-orders')
    expect(within(row).getByTestId('footprint-orders-db_time')).toHaveTextContent(
      'not measured yet')
    expect(within(row).getByTestId('footprint-orders-schema')).toHaveTextContent(
      'not measured yet')
    expect(row).not.toHaveTextContent(/\b0 ms/)
    expect(screen.getByTestId('footprint-cpu')).toHaveTextContent('not measured yet')
    expect(row).toHaveTextContent('No component breakdown yet')
  })

  it('shows figures against budget and the per-component rows', () => {
    render(<Footprint database="orders" />)
    expect(screen.getByTestId('footprint-orders-db_time')).toHaveTextContent('2,400 ms/h')
    expect(screen.getByTestId('footprint-orders-db_time')).toHaveTextContent('180,000')
    expect(screen.getByTestId('footprint-orders-schema')).toHaveTextContent('8.0 MiB')
    const collector = screen.getByTestId('footprint-orders-component-collector')
    expect(collector).toHaveTextContent('1,800 ms/h')
    expect(collector).toHaveTextContent('75%')
    expect(screen.getByTestId('footprint-orders-component-sre:probe'))
      .toHaveTextContent('25%')
    expect(screen.getByTestId('footprint-orders')).toHaveTextContent('10% unattributed')
    expect(screen.getByText('SELECT count(*) FROM pg_class')).toBeInTheDocument()
  })

  it('shows one row per database in fleet mode and the process CPU once', () => {
    apiState = {
      data: payload([measuredDatabase('alpha'), unknownDatabase('beta')],
        { known: true, cpu_ms_per_cycle: 42, cpu_budget_ms_per_cycle: 600,
          top_loops: [{ loop: 'collector', busy_ms: 900, runs: 3 }] }),
      loading: false, error: null,
    }
    render(<Footprint database="all" />)
    expect(screen.getByTestId('footprint-alpha')).toBeInTheDocument()
    expect(screen.getByTestId('footprint-beta')).toBeInTheDocument()
    expect(screen.getByTestId('footprint-alpha-db_time')).toHaveTextContent('2,400 ms/h')
    expect(screen.getByTestId('footprint-beta-db_time')).toHaveTextContent('not measured yet')
    expect(screen.getAllByTestId('footprint-cpu')).toHaveLength(1)
    expect(screen.getByTestId('footprint-cpu')).toHaveTextContent('42 ms')
    expect(screen.getByTestId('footprint-cpu')).toHaveTextContent('collector')
    expect(screen.getByTestId('footprint').getAttribute('data-url'))
      .toBe('/api/v1/self-cost')
  })

  it('asks for the selected database and links to derived settings', () => {
    render(<Footprint database="orders" />)
    expect(screen.getByTestId('footprint').getAttribute('data-url'))
      .toBe('/api/v1/self-cost?database=orders')
  })

  // The dashboard routes on the hash: the link must scroll to the panel,
  // not set location.hash (that would open "Page not found").
  it('scrolls to derived settings without changing the route', () => {
    window.location.hash = '#/settings'
    const target = document.createElement('section')
    target.id = 'derived-settings'
    target.scrollIntoView = vi.fn()
    document.body.appendChild(target)
    render(<Footprint database="orders" />)
    fireEvent.click(screen.getByTestId('footprint-derived-link'))
    expect(target.scrollIntoView).toHaveBeenCalledTimes(1)
    expect(window.location.hash).toBe('#/settings')
    expect(screen.getByTestId('footprint-derived-link').getAttribute('href')).toBeNull()
    target.remove()
  })

  it('renders loading, error and empty states', () => {
    apiState = { data: null, loading: true, error: null }
    const { rerender } = render(<Footprint database="orders" />)
    expect(screen.queryByText('Footprint')).toBeNull()
    apiState = { data: null, loading: false, error: '500 Internal Server Error' }
    rerender(<Footprint database="orders" />)
    expect(screen.getByText(/500 Internal Server Error/)).toBeInTheDocument()
    apiState = { data: payload([]), loading: false, error: null }
    rerender(<Footprint database="orders" />)
    expect(screen.getByText(/No databases to show/)).toBeInTheDocument()
  })
})
