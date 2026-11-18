import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LiveEventsProvider } from '../hooks/useLiveEvents'
import { TimeRangeProvider } from '../context/TimeRangeContext'
import { Layout } from './Layout'
import { ROUTES } from '../routes'

// dashboard-07: the header time picker shows only on the pages it drives
// (the Changes ledger and the Database health chart, ux-11), labelled with what
// it scopes. The real picker and provider run; only polling is stubbed.
// No concurrent access tests: one render tree, single-threaded.

vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(() => ({ data: null, refetch: vi.fn(), updatedAt: null })),
}))

const operator = { email: 'op@example.com', role: 'operator' }
const databases = [{ name: 'orders', emergency_stopped: false, status: {} }]
const fleetData = { summary: { emergency_stopped: false }, databases }

function tree() {
  return (
    <LiveEventsProvider>
      <TimeRangeProvider>
        <Layout user={operator} databases={databases} selectedDB="orders"
          onSelectDB={() => {}} fleetData={fleetData} onFleetChanged={() => {}}>
          <p>page</p>
        </Layout>
      </TimeRangeProvider>
    </LiveEventsProvider>
  )
}

function renderAt(hash) {
  window.location.hash = hash
  return render(tree())
}

function navigate(rerender, hash) {
  window.location.hash = hash
  rerender(tree())
}

const picker = () => screen.queryByTestId('time-range-picker')
const select = () => screen.getByRole('combobox', { name: 'Time range' })

beforeEach(() => {
  localStorage.clear()
})
afterEach(() => {
  window.location.hash = ''
  localStorage.clear()
})

describe('header time picker scope', () => {
  it('renders on #/actions with the ledger scope label', () => {
    renderAt('#/actions')
    expect(picker()).not.toBeNull()
    expect(picker()).toHaveTextContent('Ledger window')
    expect(select()).toHaveValue('24h')
  })

  it('renders on the #/advanced/actions alias of Actions', () => {
    renderAt('#/advanced/actions')
    expect(picker()).toHaveTextContent('Ledger window')
  })

  it('renders on #/database with the chart scope label', () => {
    renderAt('#/database')
    expect(picker()).toHaveTextContent('Health chart window')
  })

  it.each(['#/', '#/cases', '#/slos', '#/trust', '#/facts', '#/cases?case=4'])(
    'does not render on %s, which ignores the window', hash => {
      renderAt(hash)
      expect(screen.getByText('page')).toBeInTheDocument()
      expect(picker()).toBeNull()
    })

  it('does not render on a route with no registry entry', () => {
    renderAt('#/no-such-page')
    expect(screen.getByText('page')).toBeInTheDocument()
    expect(picker()).toBeNull()
  })

  it('is declared on the registry for exactly Changes and Database', () => {
    const scoped = ROUTES.filter(r => r.usesTimeRange)
    expect(scoped.map(r => r.id)).toEqual(['actions', 'database'])
    for (const route of scoped) expect(route.timeRangeLabel).toMatch(/window$/)
  })
})

describe('header time picker state', () => {
  it('keeps the chosen range from Actions to Cases and back', () => {
    const { rerender } = renderAt('#/actions')
    fireEvent.change(select(), { target: { value: '7d' } })
    expect(localStorage.getItem('pg_sage_range')).toBe('7d')
    navigate(rerender, '#/cases')
    expect(picker()).toBeNull()
    expect(localStorage.getItem('pg_sage_range')).toBe('7d')
    navigate(rerender, '#/actions')
    expect(select()).toHaveValue('7d')
    expect(screen.getByRole('option', { name: 'Last 7d' }).selected).toBe(true)
    expect(localStorage.getItem('pg_sage_range')).toBe('7d')
  })

  it('shows a stored range on first render', () => {
    localStorage.setItem('pg_sage_range', '6h')
    renderAt('#/actions')
    expect(select()).toHaveValue('6h')
  })

  it('falls back to Last 24h for a stored unknown range key', () => {
    localStorage.setItem('pg_sage_range', 'bogus')
    renderAt('#/actions')
    expect(select()).toHaveValue('24h')
    expect(screen.getByRole('option', { name: 'Last 24h' }).selected).toBe(true)
  })
})
