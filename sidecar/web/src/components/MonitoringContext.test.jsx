import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { MonitoringContext } from './MonitoringContext'

const db = (name, mode, extra = {}) => ({ name, status: { trust_level: mode, connected: true, ...extra } })

describe('MonitoringContext', () => {
  it('makes observation restrictions visible without equating connection to complete evidence', () => {
    render(<MonitoringContext database="orders" fleetData={{ databases: [db('orders', 'observation')] }} />)
    expect(screen.getByTestId('monitoring-context')).toHaveTextContent('Database changes are disabled')
    expect(screen.getByTestId('monitoring-context')).toHaveTextContent('Evidence freshness is unknown')
  })

  it('does not describe a mixed fleet as entirely observation-only', () => {
    render(<MonitoringContext database="all" fleetData={{ databases: [db('orders', 'observation'), db('billing', 'autonomous')] }} />)
    expect(screen.getByTestId('monitoring-context')).toHaveTextContent('1 of 2 databases use Observation mode')
    expect(screen.queryByText(/nothing will run/)).toBeNull()
  })

  it('shows exact collection time and qualifies a failed status refresh', () => {
    render(<MonitoringContext database="orders" error="offline" fleetData={{ databases: [db('orders', 'advisory', { collector_last_run: '2026-10-30T12:00:00Z' })] }} />)
    expect(screen.getByRole('status')).toHaveTextContent('Existing cases do not establish current health')
    expect(document.querySelector('time')).toHaveAttribute('dateTime', '2026-10-30T12:00:00.000Z')
    expect(screen.queryByText(/nothing will run/)).toBeNull()
  })

  it('does not ask an already selected database to select itself when policy is unknown', () => {
    render(<MonitoringContext database="orders" fleetData={{ databases: [db('orders', null)] }} />)
    expect(screen.getByTestId('monitoring-context')).toHaveTextContent('Execution policy unknown')
    expect(screen.getByTestId('monitoring-context')).toHaveTextContent('has not reported its execution policy')
    expect(screen.queryByText(/Select a database/)).toBeNull()
  })
})

// dashboard-02: the context strip says what may run without approval now.
// No concurrent access tests: a pure render of its props.
const fam = (action_type, decision) => ({ action_type, decision })
const withFamilies = (name, mode, families, extra = {}) =>
  db(name, mode, { capabilities: { action_families: families }, ...extra })

describe('MonitoringContext autonomy', () => {
  it('names the families that can run without approval now', () => {
    render(<MonitoringContext database="orders" fleetData={{ databases: [
      withFamilies('orders', 'autonomous', [
        fam('create_index', 'execute'), fam('analyze_table', 'execute'),
        fam('vacuum_table', 'queue_for_approval'),
      ]),
    ] }} />)
    const strip = screen.getByTestId('monitoring-context')
    expect(strip).toHaveTextContent(
      'Can run without approval now: create_index, analyze_table.')
    expect(strip).not.toHaveTextContent('vacuum_table')
  })

  it('says nothing runs without approval when no family executes', () => {
    render(<MonitoringContext database="orders" fleetData={{ databases: [
      withFamilies('orders', 'advisory', [fam('analyze_table', 'queue_for_approval')]),
    ] }} />)
    expect(screen.getByTestId('monitoring-context'))
      .toHaveTextContent('Nothing runs without approval right now.')
  })

  it('keeps the generic policy sentence when families were not reported', () => {
    render(<MonitoringContext database="orders"
      fleetData={{ databases: [db('orders', 'advisory')] }} />)
    expect(screen.getByTestId('monitoring-context'))
      .toHaveTextContent('Execution depends on the action')
    expect(screen.queryByText(/without approval/)).toBeNull()
  })

  it('never lists families for an emergency-stopped database', () => {
    const row = {
      ...withFamilies('orders', 'autonomous', [fam('create_index', 'execute')]),
      emergency_stopped: true,
    }
    render(<MonitoringContext database="orders" fleetData={{ databases: [row] }} />)
    expect(screen.getByTestId('monitoring-context'))
      .toHaveTextContent('The emergency stop blocks database changes')
    expect(screen.queryByText(/Can run without approval/)).toBeNull()
  })

  it('reports how many fleet databases are emergency-stopped', () => {
    render(<MonitoringContext database="all" fleetData={{
      summary: { emergency_stopped_count: 2 },
      databases: [db('a', 'autonomous'), db('b', 'advisory'), db('c', 'observation')],
    }} />)
    const strip = screen.getByTestId('monitoring-context')
    expect(strip).toHaveTextContent('2 of 3 databases emergency-stopped')
    expect(strip).toHaveTextContent('1 of 3 databases use Observation mode')
  })

  it('omits the stop count when no database is stopped or it is missing', () => {
    for (const summary of [{ emergency_stopped_count: 0 }, {}, undefined]) {
      const { unmount } = render(<MonitoringContext database="all" fleetData={{
        summary, databases: [db('a', 'autonomous')],
      }} />)
      expect(screen.queryByText(/emergency-stopped/)).toBeNull()
      unmount()
    }
  })

  it('names the fleet databases that run each family unattended', () => {
    render(<MonitoringContext database="all" fleetData={{ databases: [
      withFamilies('a', 'autonomous', [fam('create_index', 'execute')]),
      withFamilies('b', 'observation', [fam('create_index', 'blocked')]),
    ] }} />)
    expect(screen.getByTestId('monitoring-context'))
      .toHaveTextContent('Can run without approval now: create_index on a.')
  })
})
