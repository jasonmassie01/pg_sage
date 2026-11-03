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
