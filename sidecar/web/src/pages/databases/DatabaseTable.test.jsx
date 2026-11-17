import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { DatabaseTable, TABLE_ROW_LIMIT } from './DatabaseTable'

// ux-05e (win 19): a filter above the fleet table (name, host or tag,
// case-insensitive, matched literally) instead of scrolling 500 rows.
// No concurrency test: filtering is local and synchronous.

// Rendering 100 rows is slow on a loaded CI box; the time bound that
// matters (the filter step) is asserted inside the test.
const RENDER_TIMEOUT = 30000

function db(id, name, extra = {}) {
  return { id, name, host: `${name}.example.com`, port: 5432, database_name: 'app',
    trust_level: 'advisory', execution_mode: 'approval', enabled: true, ...extra }
}

const fleet = [
  db(1, 'orders', { tags: { env: 'prod' } }),
  db(2, 'billing', { host: 'replica.internal', tags: { team: 'fin' } }),
  db(3, 'analytics'),
  db(4, 'a.*b'),
]

function renderTable(databases = fleet) {
  return render(<DatabaseTable databases={databases} onEdit={vi.fn()} onDelete={vi.fn()}
    onError={vi.fn()} />)
}

const names = () => screen.queryAllByTestId('db-row').map(r => r.cells[0].textContent)
const filter = () => screen.getByRole('searchbox', { name: 'Filter databases' })

describe('DatabaseTable filter', () => {
  it('filters to the database whose name matches', () => {
    renderTable()
    expect(names()).toEqual(['orders', 'billing', 'analytics', 'a.*b'])
    fireEvent.change(filter(), { target: { value: 'ord' } })
    expect(names()).toEqual(['orders'])
    fireEvent.change(filter(), { target: { value: 'ORD' } })
    expect(names()).toEqual(['orders'])
  })

  it('matches the host and the tags', () => {
    renderTable()
    fireEvent.change(filter(), { target: { value: 'replica.internal' } })
    expect(names()).toEqual(['billing'])
    fireEvent.change(filter(), { target: { value: 'prod' } })
    expect(names()).toEqual(['orders'])
    fireEvent.change(filter(), { target: { value: 'team=fin' } })
    expect(names()).toEqual(['billing'])
  })

  it('matches regular expression characters literally', () => {
    renderTable()
    fireEvent.change(filter(), { target: { value: '.*' } })
    expect(names()).toEqual(['a.*b'])
  })

  it('says no database matches instead of an empty table', () => {
    renderTable()
    fireEvent.change(filter(), { target: { value: 'zzz-nothing' } })
    expect(names()).toEqual([])
    expect(screen.getByTestId('db-filter-empty')).toHaveTextContent('No database matches')
    expect(screen.getByTestId('db-filter-empty')).toHaveTextContent('zzz-nothing')
  })

  it('restores the full list when the filter is cleared', () => {
    renderTable()
    fireEvent.change(filter(), { target: { value: 'bill' } })
    expect(names()).toEqual(['billing'])
    fireEvent.change(filter(), { target: { value: '' } })
    expect(names()).toEqual(['orders', 'billing', 'analytics', 'a.*b'])
    expect(screen.queryByTestId('db-filter-empty')).toBeNull()
    expect(screen.queryByTestId('db-filter-count')).toBeNull()
  })

  it('keeps the no-databases message when there are none at all', () => {
    renderTable([])
    expect(screen.queryByRole('searchbox')).toBeNull()
    expect(screen.getByText(/No databases configured yet/)).toBeTruthy()
  })

  it('bounds the rendered rows of 500 and filters them in under 100 ms', () => {
    const many = Array.from({ length: 500 }, (_, i) =>
      db(i + 1, `db-${String(i).padStart(4, '0')}`))
    renderTable(many)
    expect(TABLE_ROW_LIMIT).toBeGreaterThan(0)
    expect(TABLE_ROW_LIMIT).toBeLessThan(500)
    expect(names()).toHaveLength(TABLE_ROW_LIMIT)
    expect(screen.getByTestId('db-filter-count'))
      .toHaveTextContent(`Showing ${TABLE_ROW_LIMIT} of 500`)
    const start = performance.now()
    fireEvent.change(filter(), { target: { value: 'db-0421' } })
    const took = performance.now() - start
    expect(names()).toEqual(['db-0421'])
    expect(took).toBeLessThan(100)
    // A filter matching at most the limit shows every match, no notice.
    fireEvent.change(filter(), { target: { value: 'db-04' } })
    expect(names()).toHaveLength(100)
    expect(names()[0]).toBe('db-0400')
  }, RENDER_TIMEOUT)

  it('shows every row at exactly the limit without the notice', () => {
    const exact = Array.from({ length: TABLE_ROW_LIMIT }, (_, i) => db(i + 1, `n${i}`))
    renderTable(exact)
    expect(names()).toHaveLength(TABLE_ROW_LIMIT)
    expect(screen.queryByTestId('db-filter-count')).toBeNull()
  }, RENDER_TIMEOUT)
})
