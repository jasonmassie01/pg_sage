import { fireEvent, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { FirstLookPanel } from './FirstLookPanel'
import { FLEET_SUMMARY_FROM } from '../../lib/fleetScope'

// ux-10b: in the "all" scope of a fleet of FLEET_SUMMARY_FROM or more, the
// first look is counts by grade with the worst 10 listed, not one report
// per database. A smaller fleet and one database keep the full reports.
// No concurrent access tests: a pure render of one API answer.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))

vi.mock('../../hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
}))

const item = (severity, i) => ({ rule: `r-${severity}-${i}`, severity, object: `o${i}`,
  title: `Finding ${severity} ${i}`, evidence: [] })
const name = i => `db-${String(i).padStart(3, '0')}`

// entryOf is a first-look entry with the given findings.
function entryOf(i, { critical = 0, warning = 0, info = 0, error, pending, degraded } = {}) {
  if (error) return { database: name(i), error }
  if (pending) return { database: name(i) }
  const items = [
    ...Array.from({ length: critical }, (_, k) => item('critical', k)),
    ...Array.from({ length: warning }, (_, k) => item('warning', k)),
    ...Array.from({ length: info }, (_, k) => item('info', k)),
  ]
  const checks = degraded ? [{ rule: 'x', status: 'degraded', note: 'no access' }] : []
  return { database: name(i), report: { database: name(i), duration_ms: 5, relations: 1,
    items, checks } }
}

function respond(entries) {
  api.useAPI.mockImplementation(() => ({
    data: entries === null ? { databases: null } : { databases: entries },
    loading: false, error: null, refetch: vi.fn(),
  }))
}

const summary = () => screen.getByTestId('first-look-summary')
const worst = () => within(summary()).queryAllByTestId('first-look-worst-row')

describe('FirstLookPanel fleet summary', () => {
  beforeEach(() => api.useAPI.mockReset())

  it('counts 500 databases by grade and lists the worst 10 plus the rest', () => {
    const entries = Array.from({ length: 500 }, (_, i) => {
      if (i === 40) return entryOf(i, { critical: 3, warning: 1 })
      if (i === 41) return entryOf(i, { critical: 3, warning: 4 })
      if (i === 42) return entryOf(i, { critical: 1 })
      if (i === 43) return entryOf(i, { error: 'connection refused' })
      return entryOf(i, { warning: 1 + (i % 3) })
    })
    respond(entries)
    render(<FirstLookPanel database="all" />)
    const grades = within(summary()).getByRole('list', { name: 'First look by grade' })
    expect(within(grades).getAllByRole('listitem').map(li => li.textContent)).toEqual([
      'Critical: 3', 'Warning: 496', 'Could not read: 1'])
    const rows = worst()
    expect(rows).toHaveLength(10)
    expect(rows.slice(0, 3).map(r => r.dataset.database)).toEqual([name(41), name(40), name(42)])
    expect(rows[0]).toHaveTextContent('3 critical, 4 warning')
    expect(summary()).toHaveTextContent('490 more')
    // No per-database report is rendered.
    expect(screen.queryByText(/Finding critical/)).toBeNull()
    expect(document.querySelectorAll('[data-db-block]')).toHaveLength(0)
  })

  it('opens a listed database through the scope', () => {
    const onSelectDatabase = vi.fn()
    respond(Array.from({ length: 8 }, (_, i) => entryOf(i, { critical: i === 5 ? 1 : 0 })))
    render(<FirstLookPanel database="all" onSelectDatabase={onSelectDatabase} />)
    fireEvent.click(within(worst()[0]).getByRole('button', { name: name(5) }))
    expect(onSelectDatabase).toHaveBeenCalledWith(name(5))
  })

  it('says when no database needs attention and lists none', () => {
    respond(Array.from({ length: 20 }, (_, i) => entryOf(i, { info: i % 2 })))
    render(<FirstLookPanel database="all" />)
    expect(within(summary()).getByRole('list', { name: 'First look by grade' }))
      .toHaveTextContent('Info only: 10Clean: 10')
    expect(worst()).toHaveLength(0)
    expect(summary()).toHaveTextContent('No database needs attention.')
    expect(summary()).not.toHaveTextContent('more')
  })

  it('grades running and incomplete first looks without calling them clean', () => {
    respond([
      ...Array.from({ length: 6 }, (_, i) => entryOf(i)),
      entryOf(6, { pending: true }), entryOf(7, { degraded: true }),
    ])
    render(<FirstLookPanel database="all" />)
    expect(within(summary()).getByRole('list', { name: 'First look by grade' }))
      .toHaveTextContent('Checks could not run: 1Running: 1Clean: 6')
    expect(worst().map(r => r.dataset.database)).toEqual([name(7), name(6)])
  })

  it('keeps the full reports below the threshold', () => {
    respond(Array.from({ length: FLEET_SUMMARY_FROM - 1 }, (_, i) =>
      entryOf(i, { critical: 1 })))
    render(<FirstLookPanel database="all" />)
    expect(screen.queryByTestId('first-look-summary')).toBeNull()
    expect(screen.getAllByText('Finding critical 0')).toHaveLength(FLEET_SUMMARY_FROM - 1)
    expect(document.querySelectorAll('[data-db-block]')).toHaveLength(FLEET_SUMMARY_FROM - 1)
  })

  it('summarizes from exactly the threshold', () => {
    respond(Array.from({ length: FLEET_SUMMARY_FROM }, (_, i) => entryOf(i, { critical: 1 })))
    render(<FirstLookPanel database="all" />)
    expect(worst()).toHaveLength(FLEET_SUMMARY_FROM)
    expect(summary()).not.toHaveTextContent('more')
  })

  it('keeps the full report of one selected database', () => {
    respond([entryOf(1, { critical: 1 })])
    render(<FirstLookPanel database={name(1)} />)
    expect(screen.queryByTestId('first-look-summary')).toBeNull()
    expect(screen.getByText('Finding critical 0')).toBeInTheDocument()
  })

  it('renders nothing for a null or empty fleet', () => {
    respond(null)
    const { container, unmount } = render(<FirstLookPanel database="all" />)
    expect(container.innerHTML).toBe('')
    unmount()
    respond([])
    const second = render(<FirstLookPanel database="all" />)
    expect(second.container.innerHTML).toBe('')
  })
})
