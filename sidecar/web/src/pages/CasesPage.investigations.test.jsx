import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

// Sage SRE M2: a case shows the investigation started from it, matched
// by case id and database; a case without one shows none. sre-09: the
// page asks for the visible cases' own investigations by id (100 ids a
// request), so a case whose investigation is older than the database's
// newest 100 still shows it.

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

function requestedCaseIDs(url) {
  return new URLSearchParams(url.split('?')[1] || '').getAll('case_id')
}

vi.mock('../hooks/useAPI', () => ({
  useAPI: url => {
    urls.push(url)
    let data = null
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
