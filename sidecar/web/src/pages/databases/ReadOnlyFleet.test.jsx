import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ReadOnlyFleet, ReadOnlyFleetTable } from './ReadOnlyFleet'

// ux-10a review: the read-only fleet (YAML fleet mode) is searched,
// tag-filtered, sorted and paged by the sidecar like the managed table,
// without row selection. The fake server filters by name substring and
// key=value tags, sorts by name or health and pages by index.
// No concurrency test: one table, single-threaded render; the server's
// paging under a reload is tested in internal/api.

// SLOW: a loaded box renders 50-row pages slowly.
const SLOW = { timeout: 10000 }

function runtime(name, health = 90, tags = []) {
  return { name, tags, status: { connected: true, health_score: health, findings_open: 0 } }
}

function fakeServer(rows, { fail = false } = {}) {
  const calls = []
  globalThis.fetch = vi.fn(async url => {
    calls.push(url)
    if (fail) return { ok: false, status: 502, statusText: 'Bad Gateway', text: async () => '' }
    const params = new URLSearchParams(url.split('?')[1])
    const q = (params.get('q') || '').toLowerCase()
    const tags = params.getAll('tag').map(t => t.toLowerCase())
    let hits = rows.filter(r => (!q || r.name.includes(q)) && tags.every(t => r.tags.includes(t)))
    const desc = params.get('order') === 'desc'
    const key = params.get('sort') === 'health' ? r => r.status.health_score : r => r.name
    hits = [...hits].sort((a, b) => (key(a) < key(b) ? -1 : key(a) > key(b) ? 1 : 0) *
      (desc ? -1 : 1))
    const start = Number(params.get('cursor') || 0)
    const limit = Number(params.get('limit'))
    const next = start + limit < hits.length ? String(start + limit) : ''
    return { ok: true, status: 200, json: async () => ({ mode: 'fleet', summary: {},
      databases: hits.slice(start, start + limit), total: hits.length, total_capped: false,
      next_cursor: next }) }
  })
  return calls
}

const fleet = [runtime('orders', 40, ['tier=prod']), runtime('billing', 90, ['tier=dev']),
  runtime('analytics', 70, ['tier=prod'])]

const names = () => {
  const list = screen.queryByTestId('yaml-fleet-list')
  return list ? within(list).queryAllByRole('row').slice(1).map(r => r.cells[0].textContent) : []
}

afterEach(() => { vi.restoreAllMocks() })

describe('ReadOnlyFleetTable', () => {
  it('asks the server for a sorted page and offers no selection', async () => {
    const calls = fakeServer(fleet)
    render(<ReadOnlyFleetTable databases={fleet} />)
    await waitFor(() => expect(names()).toEqual(['analytics', 'billing', 'orders']))
    expect(calls[0]).toBe('/api/v1/databases?sort=name&order=asc&limit=50')
    expect(screen.getByTestId('db-filter-count')).toHaveTextContent('Showing 3 of 3 matching')
    expect(screen.queryAllByRole('checkbox')).toHaveLength(0)
    expect(screen.queryByTestId('db-bulk-bar')).toBeNull()
  })

  it('searches and tag-filters on the server, then clears to the empty state', async () => {
    const calls = fakeServer(fleet)
    render(<ReadOnlyFleetTable databases={fleet} />)
    await waitFor(() => expect(names()).toHaveLength(3))
    const tag = screen.getByRole('textbox', { name: 'Add tag filter' })
    fireEvent.change(tag, { target: { value: 'tier=prod' } })
    fireEvent.keyDown(tag, { key: 'Enter' })
    await waitFor(() => expect(names()).toEqual(['analytics', 'orders']))
    expect(calls.at(-1)).toContain('tag=tier%3Dprod')
    fireEvent.change(screen.getByRole('searchbox', { name: 'Search databases' }),
      { target: { value: 'zzz' } })
    expect(await screen.findByTestId('db-filter-empty', {}, SLOW))
      .toHaveTextContent('No databases match; clear filters')
    fireEvent.click(screen.getByTestId('db-filter-clear'))
    await waitFor(() => expect(names()).toHaveLength(3), SLOW)
    expect(calls.at(-1)).toBe('/api/v1/databases?sort=name&order=asc&limit=50')
  })

  it('sorts by Database and Health headers with aria-sort', async () => {
    const calls = fakeServer(fleet)
    render(<ReadOnlyFleetTable databases={fleet} />)
    await waitFor(() => expect(names()).toHaveLength(3))
    const th = label => screen.getByTestId(`db-sort-${label}`).closest('th')
    expect(th('Database')).toHaveAttribute('aria-sort', 'ascending')
    fireEvent.click(screen.getByTestId('db-sort-Database'))
    await waitFor(() => expect(names()).toEqual(['orders', 'billing', 'analytics']))
    expect(th('Database')).toHaveAttribute('aria-sort', 'descending')
    fireEvent.click(screen.getByTestId('db-sort-Health'))
    await waitFor(() => expect(names()).toEqual(['orders', 'analytics', 'billing']))
    expect(calls.at(-1)).toContain('sort=health&order=asc')
    expect(th('Database')).toHaveAttribute('aria-sort', 'none')
  })

  it('loads more pages through the cursor with no repeated row', async () => {
    const big = Array.from({ length: 120 }, (_, i) => runtime(`db-${String(i).padStart(3, '0')}`))
    const calls = fakeServer(big)
    render(<ReadOnlyFleetTable databases={big} />)
    await waitFor(() => expect(names()).toHaveLength(50))
    expect(screen.getByTestId('db-filter-count')).toHaveTextContent('Showing 50 of 120')
    fireEvent.click(screen.getByTestId('db-load-more'))
    await waitFor(() => expect(names()).toHaveLength(100), SLOW)
    fireEvent.click(screen.getByTestId('db-load-more'))
    await waitFor(() => expect(names()).toHaveLength(120), SLOW)
    expect(new Set(names()).size).toBe(120)
    expect(names().at(-1)).toBe('db-119')
    expect(calls.some(u => u.includes('cursor=100'))).toBe(true)
    expect(screen.queryByTestId('db-load-more')).toBeNull()
  }, 30000)

  it('shows every database with a status line when the list fails', async () => {
    fakeServer(fleet, { fail: true })
    render(<ReadOnlyFleetTable databases={fleet} />)
    const line = await screen.findByTestId('db-list-error', {}, SLOW)
    expect(line).toHaveAttribute('role', 'status')
    expect(line).toHaveTextContent('showing every database')
    expect(names()).toEqual(['orders', 'billing', 'analytics'])
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('renders and fetches nothing for an empty fleet', () => {
    const calls = fakeServer([])
    const { container } = render(<ReadOnlyFleetTable databases={[]} />)
    expect(container).toBeEmptyDOMElement()
    expect(calls).toHaveLength(0)
  })
})

describe('ReadOnlyFleet', () => {
  it('keeps plain headers without a sort', () => {
    render(<ReadOnlyFleet databases={fleet} testId="config-file-databases" />)
    expect(screen.queryAllByRole('button')).toHaveLength(0)
    expect(screen.getByTestId('config-file-databases')).toHaveTextContent('orders')
  })
})
