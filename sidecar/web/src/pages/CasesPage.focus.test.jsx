import { act, render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

// mcp-05: Ask Sage citations deep-link here as finding=<id> or
// investigation=<id>. The page focuses the case the finding or
// investigation belongs to, and says so plainly when there is none.
// No concurrency tests: the page reads its focus from the hash on render.

const orders = {
  id: 'finding:orders:missing_index:table:public.orders', source_type: 'finding',
  source_ids: ['42'], database_name: 'orders', title: 'Missing index on public.orders',
  severity: 'warning', state: 'open', why_now: 'not urgent', evidence: [],
}
const forecast = {
  id: 'forecast:orders:disk', source_type: 'forecast', source_ids: ['43'],
  database_name: 'orders', title: 'Disk fills in 9 days', severity: 'warning',
  state: 'open', why_now: 'trend', evidence: [],
}
const hint = {
  id: 'query_hint:orders:777', source_type: 'query_hint', source_ids: ['777'],
  database_name: 'orders', title: 'Query hint 777', severity: 'info', state: 'open',
  why_now: 'hint', evidence: [],
}
// Investigation ids are canonical UUIDs (internal/sre/ids.go).
const INV = '9b2e0c4a-1d3f-4a5b-8c6d-7e8f9a0b1c2d'
const INV_OTHER = '6f1c2d3e-4a5b-4c6d-9e7f-8a9b0c1d2e3f'
const otherDB = { ...orders, id: 'finding:billing:missing_index:table:public.orders',
  database_name: 'billing', title: 'Missing index on billing orders' }
let cases
let investigations
const urls = []
// cases-12: a cited investigation is read from its per-id route; one not
// in `investigations` answers 404, as the server does.
function investigationDetail(url) {
  const [, db, id] = url.match(/^\/api\/v1\/databases\/([^/]+)\/investigations\/([^/?]+)$/)
  const inv = investigations.find(i => i.id === id && i.database === db)
  return inv ? { data: { database: db, investigation: inv }, loading: false, error: null }
    : { data: null, loading: false, error: 'investigation not found (404)', errorStatus: 404 }
}
vi.mock('../hooks/useAPI', () => ({
  useAPI: url => {
    urls.push(url)
    if (url?.includes('/investigations/')) return { ...investigationDetail(url), refetch: vi.fn() }
    return { data: url?.startsWith('/api/v1/cases') ? { cases } : { items: [] },
      loading: false, error: null, refetch: vi.fn() }
  },
}))
vi.mock('../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

beforeEach(() => {
  cases = [orders, forecast, hint, otherDB]
  investigations = []
  urls.length = 0
})

const go = hash => window.history.replaceState(null, '', hash)
const focusedCase = () => new URLSearchParams(screen.getByRole('link',
  { name: 'Explain this' }).getAttribute('href').split('?')[1]).get('case')

describe('Cases focus from an Ask Sage citation', () => {
  it('focuses the case whose source_ids hold the finding', () => {
    go('#/cases?finding=42&database=orders')
    render(<CasesPage database="orders" />)
    expect(screen.getAllByRole('article')).toHaveLength(1)
    expect(screen.getByRole('link', { name: 'Back to all issues' })).toBeVisible()
    expect(screen.queryByTestId('cases-focus-missing')).toBeNull()
    expect(focusedCase()).toBe(orders.id)
  })

  it('focuses a forecast case backed by the finding', () => {
    go('#/cases?finding=43&database=orders')
    render(<CasesPage database="orders" />)
    expect(screen.getAllByRole('article')).toHaveLength(1)
    expect(focusedCase()).toBe(forecast.id)
  })

  it('never resolves a finding id through a case that is not finding-backed', () => {
    go('#/cases?finding=777&database=orders')
    render(<CasesPage database="orders" />)
    expect(screen.queryByRole('article')).toBeNull()
    expect(screen.getByTestId('cases-focus-missing')).toHaveTextContent('record 777')
  })

  it('does not borrow another database case for the finding', () => {
    go('#/cases?finding=42&database=billing')
    const view = render(<CasesPage database="all" />)
    expect(screen.getAllByRole('article')).toHaveLength(1)
    expect(focusedCase()).toBe(otherDB.id)
    view.unmount()
    cases = [orders]
    render(<CasesPage database="all" />)
    expect(screen.queryByRole('article')).toBeNull()
    expect(screen.getByTestId('cases-focus-missing')).toBeVisible()
  })

  it('names a finding with no case and links to its record, not an empty focus', () => {
    go('#/cases?finding=999&database=orders')
    render(<CasesPage database="orders" />)
    expect(screen.queryByRole('article')).toBeNull()
    const notice = screen.getByTestId('cases-focus-missing')
    expect(notice).toHaveTextContent(/record 999/)
    expect(notice).toHaveTextContent(/resolved/)
    expect(within(notice).getByRole('link', { name: /source record/i }))
      .toHaveAttribute('href', '/api/v1/findings/999?database=orders')
    expect(screen.queryByText(/This case is unavailable/)).toBeNull()
  })

  it('encodes a hostile finding reference in the record link', () => {
    go(`#/cases?finding=${encodeURIComponent('1/../../x?y')}&database=orders`)
    render(<CasesPage database="orders" />)
    expect(within(screen.getByTestId('cases-focus-missing'))
      .getByRole('link', { name: /source record/i }))
      .toHaveAttribute('href', '/api/v1/findings/1%2F..%2F..%2Fx%3Fy?database=orders')
  })

  it('focuses the case of a cited investigation', () => {
    investigations = [{ id: INV_OTHER, case_id: forecast.id, database: 'orders' },
      { id: INV, case_id: orders.id, database: 'orders' }]
    go(`#/cases?investigation=${INV}&database=orders`)
    render(<CasesPage database="orders" />)
    expect(screen.getAllByRole('article')).toHaveLength(1)
    expect(focusedCase()).toBe(orders.id)
    expect(urls.some(u => u.startsWith('/api/v1/cases') &&
      u.includes(`case=${encodeURIComponent(orders.id)}`))).toBe(true)
  })

  it('says when a cited investigation is not loaded', () => {
    go('#/cases?investigation=gone&database=orders')
    render(<CasesPage database="orders" />)
    expect(screen.queryByRole('article')).toBeNull()
    expect(screen.getByTestId('cases-focus-missing')).toHaveTextContent('investigation gone')
  })

  it('never asks the server for the ask: case of an investigation Ask Sage opened', () => {
    // An investigation Ask Sage starts has a synthetic ask:<digest> case id
    // that names no case; the cases API rejects it as a focus (400), which
    // would blank the page instead of saying what happened.
    investigations = [{ id: INV, case_id: 'ask:0123456789abcdef', database: 'orders' }]
    go(`#/cases?investigation=${INV}&database=orders`)
    render(<CasesPage database="orders" />)
    expect(urls.some(u => u.startsWith('/api/v1/cases') && u.includes('case='))).toBe(false)
    expect(screen.queryByRole('article')).toBeNull()
    const notice = screen.getByTestId('cases-focus-missing')
    expect(notice).toHaveTextContent(new RegExp(`investigation ${INV}`, 'i'))
    expect(notice).toHaveTextContent(/no issue/)
    expect(within(notice).getByRole('link', { name: /investigation record/i }))
      .toHaveAttribute('href', `/api/v1/databases/orders/investigations/${INV}`)
  })

  it('follows a hash change from a finding focus back to the list', () => {
    go('#/cases?finding=42&database=orders')
    render(<CasesPage database="orders" />)
    expect(screen.getAllByRole('article')).toHaveLength(1)
    act(() => {
      go('#/cases?database=orders')
      window.dispatchEvent(new Event('hashchange'))
    })
    expect(screen.getAllByRole('article').length).toBeGreaterThan(1)
  })

  it('keeps an explicit case focus working', () => {
    go(`#/cases?case=${encodeURIComponent(hint.id)}&database=orders`)
    render(<CasesPage database="orders" />)
    expect(screen.getAllByRole('article')).toHaveLength(1)
    expect(focusedCase()).toBe(hint.id)
  })
})
