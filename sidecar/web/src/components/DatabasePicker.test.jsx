import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabasePicker, PICKER_FILTER_FROM } from './DatabasePicker'

// ux-10b: the header scope picker. A small fleet (one database included:
// the plan said one hides it "as today", but today shows it and
// ask_route.test.jsx pins that) keeps the plain select (the "Database
// scope" control every spec drives),
// and from PICKER_FILTER_FROM up it is an ARIA 1.2 combobox served by the
// fleet list's q and tag (ux-10a), 50 names a page, with recent databases,
// tag scopes and "All databases". It replaces ux-05e's find box.

const fleet = n => Array.from({ length: n }, (_, i) => ({ name: `db-${i}` }))
const big = [...fleet(PICKER_FILTER_FROM + 4), { name: 'payments' }, { name: 'payroll' }]

// reply is a fetch Response for one fleet list page.
function reply(names, { next = '', total = names.length, status = 200 } = {}) {
  const body = status === 200
    ? { databases: names.map(name => ({ name })), total, next_cursor: next }
    : { error: 'list unavailable' }
  return {
    ok: status === 200, status, statusText: '',
    json: async () => body, text: async () => JSON.stringify(body),
  }
}

// searchServer answers like GET /api/v1/databases?q=&tag=&cursor=.
function searchServer(names = big.map(d => d.name)) {
  return vi.fn(async url => {
    const params = new URL(url, 'http://x').searchParams
    const q = (params.get('q') || '').toLowerCase()
    return reply(names.filter(n => n.includes(q)))
  })
}

function renderPicker({ list = big, selected = 'all', selectedTag = '', ...props } = {}) {
  const onSelect = vi.fn()
  const onSelectTag = vi.fn()
  const utils = render(<DatabasePicker databases={list} selected={selected}
    selectedTag={selectedTag} onSelect={onSelect} onSelectTag={onSelectTag}
    data-testid="database-picker" {...props} />)
  return { ...utils, onSelect, onSelectTag }
}

const scope = () => screen.getByRole('combobox', { name: 'Database scope' })
const optionNames = () => screen.queryAllByRole('option').map(o => o.textContent)

async function typeInto(text) {
  const input = scope()
  fireEvent.focus(input)
  fireEvent.change(input, { target: { value: text } })
}

beforeEach(() => {
  localStorage.clear()
  globalThis.fetch = searchServer()
})
afterEach(() => vi.restoreAllMocks())

describe('DatabasePicker sizes', () => {
  it('keeps a single database as a plain select naming it, as today', () => {
    renderPicker({ list: [{ name: 'orders' }], selected: 'orders' })
    expect(scope().tagName).toBe('SELECT')
    expect(scope()).toHaveValue('orders')
    expect(Array.from(scope().options).map(o => o.value)).toEqual(['all', 'orders'])
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })

  it('renders nothing for no databases', () => {
    const { container } = renderPicker({ list: [] })
    expect(container.innerHTML).toBe('')
  })

  it('keeps the plain select for a small fleet', () => {
    const small = fleet(PICKER_FILTER_FROM - 1)
    const { onSelect } = renderPicker({ list: small })
    const select = scope()
    expect(select.tagName).toBe('SELECT')
    expect(Array.from(select.options).map(o => o.value))
      .toEqual(['all', ...small.map(d => d.name)])
    fireEvent.change(select, { target: { value: 'db-2' } })
    expect(onSelect).toHaveBeenCalledWith('db-2')
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })

  it('becomes a combobox from the threshold up, closed and naming the scope', () => {
    renderPicker({ list: fleet(PICKER_FILTER_FROM), selected: 'db-3' })
    const input = scope()
    expect(input.tagName).toBe('INPUT')
    expect(input).toHaveAttribute('aria-expanded', 'false')
    expect(input).toHaveAttribute('aria-autocomplete', 'list')
    expect(input).toHaveValue('db-3')
    expect(screen.queryByRole('listbox')).toBeNull()
    expect(screen.getByTestId('database-picker')).toBe(input)
  })

  it('names the all and tag scopes in the closed combobox', () => {
    const { unmount } = renderPicker()
    expect(scope()).toHaveValue('All databases')
    unmount()
    renderPicker({ selectedTag: 'tier=prod' })
    expect(scope()).toHaveValue('tag:tier=prod')
  })
})

describe('DatabasePicker search', () => {
  it('lists matching databases from the server and Enter selects one', async () => {
    const { onSelect } = renderPicker()
    await typeInto('pay')
    await waitFor(() => expect(optionNames()).toContain('payroll'))
    expect(globalThis.fetch).toHaveBeenLastCalledWith(
      '/api/v1/databases?q=pay&sort=name&order=asc&limit=50',
      expect.objectContaining({ signal: expect.any(AbortSignal) }))
    expect(optionNames()).toEqual(['All databases', 'payments', 'payroll'])
    expect(scope()).toHaveAttribute('aria-expanded', 'true')
    fireEvent.keyDown(scope(), { key: 'ArrowDown' })
    fireEvent.keyDown(scope(), { key: 'ArrowDown' })
    const active = scope().getAttribute('aria-activedescendant')
    expect(document.getElementById(active)).toHaveTextContent('payments')
    expect(document.getElementById(active)).toHaveAttribute('aria-selected', 'true')
    fireEvent.keyDown(scope(), { key: 'Enter' })
    expect(onSelect).toHaveBeenCalledTimes(1)
    expect(onSelect).toHaveBeenCalledWith('payments')
    expect(scope()).toHaveAttribute('aria-expanded', 'false')
  })

  it('selects the exact or single match on Enter with nothing highlighted', async () => {
    const { onSelect } = renderPicker()
    await typeInto('PAYROLL')
    await waitFor(() => expect(optionNames()).toContain('payroll'))
    fireEvent.keyDown(scope(), { key: 'Enter' })
    expect(onSelect).toHaveBeenCalledWith('payroll')
  })

  it('does nothing on Enter when the match is ambiguous', async () => {
    const { onSelect } = renderPicker()
    await typeInto('pay')
    await waitFor(() => expect(optionNames()).toContain('payroll'))
    fireEvent.keyDown(scope(), { key: 'Enter' })
    expect(onSelect).not.toHaveBeenCalled()
    expect(scope()).toHaveAttribute('aria-expanded', 'true')
  })

  it('selects with the mouse and offers All databases', async () => {
    const { onSelect } = renderPicker({ selected: 'payments' })
    await typeInto('pay')
    await waitFor(() => expect(optionNames()).toContain('payroll'))
    fireEvent.click(screen.getByRole('option', { name: 'All databases' }))
    expect(onSelect).toHaveBeenCalledWith('all')
  })

  it('says when no database matches', async () => {
    renderPicker()
    await typeInto('zzz')
    expect(await screen.findByText('No database matches')).toBeInTheDocument()
    expect(optionNames()).toEqual(['All databases'])
  })

  it('keeps the scope and shows an inline error when the search fails', async () => {
    globalThis.fetch = vi.fn(async () => reply([], { status: 503 }))
    const { onSelect } = renderPicker({ selected: 'db-1' })
    await typeInto('pay')
    const alert = await screen.findByRole('alert')
    expect(within(screen.getByRole('listbox')).getByRole('alert')).toBe(alert)
    expect(alert).toHaveTextContent('list unavailable')
    expect(alert).toHaveTextContent('scope is unchanged')
    fireEvent.keyDown(scope(), { key: 'Enter' })
    expect(onSelect).not.toHaveBeenCalled()
    fireEvent.keyDown(scope(), { key: 'Escape' })
    expect(scope()).toHaveValue('db-1')
    expect(scope()).toHaveAttribute('aria-expanded', 'false')
  })

  it('escapes the typed text into the query', async () => {
    renderPicker()
    await typeInto('a&b')
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
    expect(globalThis.fetch.mock.calls.at(-1)[0])
      .toBe('/api/v1/databases?q=a%26b&sort=name&order=asc&limit=50')
  })
})

describe('DatabasePicker concurrency', () => {
  it('aborts stale searches and shows only the last answer', async () => {
    const pending = []
    globalThis.fetch = vi.fn((url, init) => new Promise(resolve => {
      pending.push({ url, init, resolve })
    }))
    renderPicker()
    fireEvent.focus(scope())
    for (const text of ['p', 'pa', 'pay']) {
      fireEvent.change(scope(), { target: { value: text } })
    }
    const searches = pending.filter(p => p.url.includes('q='))
    expect(searches.map(p => new URL(p.url, 'http://x').searchParams.get('q')))
      .toEqual(['p', 'pa', 'pay'])
    expect(searches[0].init.signal.aborted).toBe(true)
    expect(searches[1].init.signal.aborted).toBe(true)
    expect(searches[2].init.signal.aborted).toBe(false)
    // The last answer lands first; the stale ones land after it.
    await act(async () => { searches[2].resolve(reply(['payments'])) })
    await act(async () => { searches[0].resolve(reply(['p-stale'])) })
    await act(async () => { searches[1].resolve(reply(['pa-stale'])) })
    expect(optionNames()).toEqual(['All databases', 'payments'])
  })
})

describe('DatabasePicker paging, recents and tags', () => {
  it('pages at 50 and loads more on request', async () => {
    const first = Array.from({ length: 50 }, (_, i) => `n-${String(i).padStart(3, '0')}`)
    globalThis.fetch = vi.fn(async url => (url.includes('cursor=c1')
      ? reply(['n-050', 'n-051'], { total: 52 })
      : reply(first, { next: 'c1', total: 52 })))
    renderPicker()
    await typeInto('n-')
    await waitFor(() => expect(optionNames()).toContain('n-049'))
    expect(screen.getAllByRole('option')).toHaveLength(1 + 50 + 1)
    expect(screen.getByText('50 of 52 shown')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('option', { name: 'Load more' }))
    await waitFor(() => expect(optionNames()).toContain('n-051'))
    expect(globalThis.fetch.mock.calls.at(-1)[0]).toContain('cursor=c1')
    expect(optionNames()).not.toContain('Load more')
    expect(scope()).toHaveAttribute('aria-expanded', 'true')
  })

  it('lists recent databases first while nothing is typed', async () => {
    localStorage.setItem('pg_sage_recent_dbs', JSON.stringify(['payroll', 'gone', 'db-2']))
    renderPicker()
    fireEvent.focus(scope())
    await waitFor(() => expect(screen.getByRole('group', { name: 'Recent' }))
      .toBeInTheDocument())
    const recent = within(screen.getByRole('group', { name: 'Recent' }))
    // A recent name no longer in the inventory is not offered.
    expect(recent.getAllByRole('option').map(o => o.textContent)).toEqual(['payroll', 'db-2'])
  })

  it('records a selection as recent', async () => {
    renderPicker()
    await typeInto('payroll')
    await waitFor(() => expect(optionNames()).toContain('payroll'))
    fireEvent.keyDown(scope(), { key: 'Enter' })
    expect(JSON.parse(localStorage.getItem('pg_sage_recent_dbs'))).toEqual(['payroll'])
  })

  it('offers a typed tag scope and lists its databases', async () => {
    globalThis.fetch = vi.fn(async url => (url.includes('tag=tier%3Dprod')
      ? reply(['payments'], { total: 1 }) : reply([])))
    const { onSelectTag, onSelect } = renderPicker()
    await typeInto('tag:tier=prod')
    const tagOption = await screen.findByRole('option',
      { name: 'All databases tagged tier=prod' })
    await waitFor(() => expect(optionNames()).toContain('payments'))
    expect(globalThis.fetch.mock.calls.at(-1)[0]).not.toContain('q=')
    fireEvent.click(tagOption)
    expect(onSelectTag).toHaveBeenCalledWith('tier=prod')
    expect(onSelect).not.toHaveBeenCalled()
  })

  it('searches nothing until it is opened', () => {
    renderPicker()
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })
})
