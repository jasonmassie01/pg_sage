import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

// Sage SRE M2: a case shows the investigation started from it, matched
// by case id and database; a case without one shows none. sre-09: the
// page asks for the visible cases' own investigations by id (100 ids a
// request), so a case whose investigation is older than the database's
// newest 100 still shows it. cases-12: a cited investigation is read by
// id from its database's per-id route, once, and never through the
// database's newest 100.

const lockCase = {
  id: 'incident:orders:lock_contention:1', source_type: 'incident',
  source_ids: ['1'], database_name: 'orders', title: 'Lock storm',
  severity: 'critical', state: 'open',
}
const otherCase = {
  id: 'finding:orders:index', source_type: 'finding', source_ids: ['2'],
  database_name: 'orders', title: 'Missing index', severity: 'warning',
  state: 'open',
}
const investigation = {
  id: 'inv-1', case_id: lockCase.id, database: 'orders', state: 'concluded',
  trigger_kind: 'lock_blocking',
  summary: { family: 'lock_blocking', conclusive: true, root: 'idle_in_tx_holder' },
}
const defaultInvestigations = [investigation, { ...investigation, id: 'inv-other',
  case_id: lockCase.id, database: 'billing' }]

let urls = []
let cases = [lockCase, otherCase]
// The server's investigations, newest first: the mock answers like the
// API, with the newest of each requested case id, or the newest 100 when
// no case id is named.
let serverInvestigations = defaultInvestigations
// While pending, an investigations URL not asked before has no answer yet
// (its request is in flight), as useAPI reports a new resource.
let pending = false
let answered = new Set()
// cases-12: the per-id investigation route answers from `details` (url ->
// a useAPI result); a url not listed answers 404, as the server does.
let details = {}
const intervals = {}
const notFound = { data: null, loading: false, error: 'investigation not found (404)',
  errorStatus: 404, refetch: () => {} }

function requestedCaseIDs(url) {
  return new URLSearchParams(url.split('?')[1] || '').getAll('case_id')
}

vi.mock('../hooks/useAPI', () => ({
  useAPI: (url, interval) => {
    urls.push(url)
    intervals[url] = interval
    let data = null
    if (url?.startsWith('/api/v1/databases/')) return details[url] ?? notFound
    if (url?.startsWith('/api/v1/investigations')) {
      if (pending && !answered.has(url)) {
        return { data: null, loading: true, error: null, refetch: vi.fn() }
      }
      answered.add(url)
      const ids = requestedCaseIDs(url)
      data = { items: ids.length === 0 ? serverInvestigations.slice(0, 100)
        : serverInvestigations.filter(inv => ids.includes(inv.case_id)) }
    } else if (url?.startsWith('/api/v1/cases')) {
      data = { cases }
    }
    return { data, loading: false, error: null, refetch: vi.fn() }
  },
}))

afterEach(() => {
  urls = []
  cases = [lockCase, otherCase]
  serverInvestigations = defaultInvestigations
  pending = false
  answered = new Set()
  details = {}
  window.history.replaceState(null, '', '#/')
})

const investigationURLs = () =>
  urls.filter(u => u?.startsWith('/api/v1/investigations'))

describe('Cases linked to investigations', () => {
  it('shows the case investigation of the same database only', () => {
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    const asked = investigationURLs()
    expect(asked.length).toBeGreaterThan(0)
    expect(asked.every(u => u.startsWith('/api/v1/investigations?database=all&')))
      .toBe(true)
    const lock = screen.getByText('Lock storm').closest('article')
    const panels = within(lock).getAllByTestId('investigation-panel')
    expect(panels).toHaveLength(1)
    expect(within(lock).getByTestId('investigation-state'))
      .toHaveTextContent('Concluded')
    const other = screen.getByText('Missing index').closest('article')
    expect(within(other).queryByTestId('investigation-panel')).toBeNull()
  })

  it('finds a case investigation older than the newest 100', () => {
    // 100 newer investigations of other cases, then the case's own.
    const newer = Array.from({ length: 100 }, (_, i) => ({ ...investigation,
      id: `inv-new-${i}`, case_id: `incident:orders:other:${i}` }))
    serverInvestigations = [...newer, { ...investigation, id: 'inv-old' }]
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    const asked = investigationURLs()
    expect(asked.some(u => requestedCaseIDs(u).includes(lockCase.id))).toBe(true)
    expect(asked.every(u => u.startsWith('/api/v1/investigations?database=orders&')))
      .toBe(true)
    const lock = screen.getByText('Lock storm').closest('article')
    expect(within(lock).getByTestId('investigation-panel')).toBeInTheDocument()
    expect(within(lock).getByTestId('investigation-state'))
      .toHaveTextContent('Concluded')
  })

  it('sends no investigations request when no case is visible', () => {
    cases = []
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    expect(investigationURLs()).toEqual([])
  })

  it('asks for 150 cases in requests of at most 100 ids, each id once', () => {
    cases = Array.from({ length: 150 }, (_, i) => ({ ...otherCase,
      id: `finding:orders:${i}`, title: `Finding ${i}` }))
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    const asked = [...new Set(investigationURLs())]
    expect(asked).toHaveLength(2)
    const ids = asked.flatMap(requestedCaseIDs)
    expect(asked.map(u => requestedCaseIDs(u).length)).toEqual([100, 50])
    expect(new Set(ids).size).toBe(150)
  })

  it('gives each database its own investigation of a shared case id', () => {
    const slo = { id: 'slo:checkout', source_type: 'incident', source_ids: ['9'],
      severity: 'critical', state: 'open' }
    cases = [{ ...slo, database_name: 'orders', title: 'Orders SLO' },
      { ...slo, database_name: 'billing', title: 'Billing SLO' }]
    serverInvestigations = [
      { ...investigation, id: 'inv-o', case_id: slo.id, database: 'orders' },
      { ...investigation, id: 'inv-b', case_id: slo.id, database: 'billing',
        state: 'inconclusive' },
    ]
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    const orders = screen.getByText('Orders SLO').closest('article')
    const billing = screen.getByText('Billing SLO').closest('article')
    expect(within(orders).getByTestId('investigation-state'))
      .toHaveTextContent('Concluded')
    expect(within(billing).getByTestId('investigation-state'))
      .toHaveTextContent('Inconclusive')
    const asked = [...new Set(investigationURLs())]
    expect(asked).toHaveLength(1)
    expect(requestedCaseIDs(asked[0])).toEqual([slo.id])
  })

  it('keeps an open investigation while a changed case set is fetched', () => {
    // A new case changes the visible ids, so the chunk asks again; until
    // that answer lands the open panel must not unmount (and collapse).
    const { rerender } = render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    const lockBefore = screen.getByText('Lock storm').closest('article')
    fireEvent.click(within(lockBefore).getByTestId('investigation-toggle'))
    pending = true
    cases = [{ ...otherCase, id: 'finding:orders:new', title: 'New finding' },
      lockCase, otherCase]
    rerender(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    expect(screen.getByText('New finding')).toBeInTheDocument()
    const lock = screen.getByText('Lock storm').closest('article')
    expect(within(lock).getByTestId('investigation-toggle'))
      .toHaveTextContent('Hide investigation')
    expect(within(lock).getByTestId('investigation-state'))
      .toHaveTextContent('Concluded')
  })
})

describe('Cases focus on a cited investigation (cases-12)', () => {
  const INV = '3f2a9c1e-7b4d-4e8a-9c2f-1a2b3c4d5e6f'
  const INV2 = '9c1e3f2a-4e8a-4b7d-8c2f-6f5e4d3c2b1a'
  const detailURL = (db, id = INV) => `/api/v1/databases/${db}/investigations/${id}`
  const answer = (inv, db = 'orders') => ({ data: { database: db,
    investigation: { ...investigation, ...inv } }, loading: false, error: null,
  refetch: vi.fn() })
  const go = hash => window.history.replaceState(null, '', hash)
  const detailURLs = () => [...new Set(urls.filter(u => u?.includes('/investigations/')))]
  // The database-wide list: an investigations read that names no case.
  const listReads = () => investigationURLs()
    .filter(u => requestedCaseIDs(u).length === 0)
  const titles = () => screen.queryAllByRole('article')
    .map(a => within(a).getByRole('heading', { level: 3 }).textContent)

  it('reads the investigation once by id and opens the case it names', () => {
    details[detailURL('orders')] = answer({ id: INV, case_id: lockCase.id })
    go(`#/cases?investigation=${INV}&database=orders`)
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    expect(detailURLs()).toEqual([detailURL('orders')])
    expect(intervals[detailURL('orders')]).toBe(0)
    expect(listReads()).toEqual([])
    expect(titles()).toEqual(['Lock storm'])
    expect(urls.some(u => u?.startsWith('/api/v1/cases') &&
      u.includes(`case=${encodeURIComponent(lockCase.id)}`))).toBe(true)
    expect(screen.queryByTestId('cases-focus-missing')).toBeNull()
  })

  it('uses the selected database when the citation names none', () => {
    details[detailURL('orders')] = answer({ id: INV, case_id: lockCase.id })
    go(`#/cases?investigation=${INV}`)
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    expect(detailURLs()).toEqual([detailURL('orders')])
    expect(titles()).toEqual(['Lock storm'])
  })

  it('lower-cases an upper-case id before the request', () => {
    details[detailURL('orders')] = answer({ id: INV, case_id: lockCase.id })
    go(`#/cases?investigation=${INV.toUpperCase()}&database=orders`)
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    expect(detailURLs()).toEqual([detailURL('orders')])
    expect(titles()).toEqual(['Lock storm'])
  })

  it('encodes the database name into one path segment', () => {
    go(`#/cases?investigation=${INV}&database=${encodeURIComponent('a/b?c')}`)
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    expect(detailURLs()).toEqual([`/api/v1/databases/a%2Fb%3Fc/investigations/${INV}`])
  })

  it.each([['..%2F..%2Fx', '../../x'], ['77', '77']])(
    'makes no request for the id %s and says so', (raw, shown) => {
      go(`#/cases?investigation=${raw}&database=orders`)
      render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
      expect(detailURLs()).toEqual([])
      expect(listReads()).toEqual([])
      expect(screen.queryByRole('article')).toBeNull()
      expect(screen.getByTestId('cases-focus-missing'))
        .toHaveTextContent(`investigation ${shown}`)
    })

  it('asks for the database when none is known, with no read', () => {
    go(`#/cases?investigation=${INV}`)
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    expect(investigationURLs()).toEqual([])
    expect(detailURLs()).toEqual([])
    expect(screen.queryByRole('article')).toBeNull()
    expect(screen.getByTestId('cases-focus-missing'))
      .toHaveTextContent('Choose the database this investigation belongs to')
  })

  it.each([[''], [null]])('opens no case for an investigation with case_id %j', id => {
    details[detailURL('orders')] = answer({ id: INV, case_id: id })
    go(`#/cases?investigation=${INV}&database=orders`)
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    expect(screen.queryByRole('article')).toBeNull()
    expect(urls.some(u => u?.startsWith('/api/v1/cases') && u.includes('case=')))
      .toBe(false)
    expect(screen.getByTestId('cases-focus-missing')).toHaveTextContent(INV)
  })

  it('links an Ask Sage investigation (ask: case id) to its record', () => {
    details[detailURL('orders')] = answer({ id: INV, case_id: 'ask:0123456789abcdef' })
    go(`#/cases?investigation=${INV}&database=orders`)
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    expect(screen.queryByRole('article')).toBeNull()
    expect(urls.some(u => u?.startsWith('/api/v1/cases') && u.includes('case=')))
      .toBe(false)
    const notice = screen.getByTestId('cases-focus-missing')
    expect(notice).toHaveTextContent(/no case here/)
    expect(within(notice).getByRole('link', { name: /investigation record/i }))
      .toHaveAttribute('href', detailURL('orders'))
  })

  it('resolves an investigation older than the newest 100 without the list', () => {
    serverInvestigations = Array.from({ length: 100 }, (_, i) => ({ ...investigation,
      id: `inv-new-${i}`, case_id: `incident:orders:other:${i}` }))
    details[detailURL('orders')] = answer({ id: INV, case_id: lockCase.id })
    go(`#/cases?investigation=${INV}&database=orders`)
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    expect(listReads()).toEqual([])
    expect(titles()).toEqual(['Lock storm'])
  })

  it('says not found for a 404, without an error banner', () => {
    go(`#/cases?investigation=${INV}&database=orders`)
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    expect(screen.getByTestId('cases-focus-missing'))
      .toHaveTextContent(`No case is loaded for investigation ${INV}`)
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull()
  })

  it('shows a 503 as an error with Retry, not as not found', () => {
    const refetch = vi.fn()
    details[detailURL('orders')] = { data: null, loading: false, refetch,
      error: 'investigation store unavailable (503)', errorStatus: 503 }
    go(`#/cases?investigation=${INV}&database=orders`)
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    expect(screen.getByRole('alert'))
      .toHaveTextContent('investigation store unavailable (503)')
    expect(screen.queryByTestId('cases-focus-missing')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('shows no notice and no case while the investigation loads', () => {
    details[detailURL('orders')] = { data: null, loading: true, error: null,
      refetch: vi.fn() }
    go(`#/cases?investigation=${INV}&database=orders`)
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    expect(screen.queryByTestId('cases-focus-missing')).toBeNull()
    expect(screen.queryByRole('article')).toBeNull()
    expect(urls.some(u => u?.startsWith('/api/v1/cases'))).toBe(false)
  })

  it('follows a hash change to another investigation with one new read', () => {
    details[detailURL('orders')] = answer({ id: INV, case_id: lockCase.id })
    details[detailURL('orders', INV2)] = answer({ id: INV2, case_id: otherCase.id })
    go(`#/cases?investigation=${INV}&database=orders`)
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    expect(titles()).toEqual(['Lock storm'])
    act(() => {
      go(`#/cases?investigation=${INV2}&database=orders`)
      window.dispatchEvent(new Event('hashchange'))
    })
    expect(detailURLs()).toEqual([detailURL('orders'), detailURL('orders', INV2)])
    expect(titles()).toEqual(['Missing index'])
  })
})
