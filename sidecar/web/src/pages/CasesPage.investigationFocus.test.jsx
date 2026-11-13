import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

// cases-12: a cited investigation read through the real useAPI and a fake
// fetch that answers like the server and, like a browser, rejects a
// request whose signal was aborted. Covers what a mocked useAPI cannot: a
// stale answer after a fast hash change, and a failed read with Retry.

const INV = '3f2a9c1e-7b4d-4e8a-9c2f-1a2b3c4d5e6f'
const INV2 = '9c1e3f2a-4e8a-4b7d-8c2f-6f5e4d3c2b1a'
const detailURL = id => `/api/v1/databases/orders/investigations/${id}`
const lockCase = {
  id: 'incident:orders:lock_contention:1', source_type: 'incident',
  source_ids: ['1'], database_name: 'orders', title: 'Lock storm',
  severity: 'critical', state: 'open',
}
const otherCase = {
  id: 'finding:orders:index', source_type: 'finding', source_ids: ['2'],
  database_name: 'orders', title: 'Missing index', severity: 'warning', state: 'open',
}
const detail = (id, caseID) => ({ database: 'orders', investigation: {
  id, case_id: caseID, state: 'concluded', trigger_kind: 'lock_blocking', summary: {} } })

let routes
let requested

const json = (body, status = 200) => new Response(JSON.stringify(body), {
  status, headers: { 'Content-Type': 'application/json' } })

// fakeFetch answers a route's handler (a body, a Response or a promise of
// one); an aborted request rejects with AbortError, as browsers do.
function fakeFetch(url, { signal } = {}) {
  requested.push(String(url))
  const path = String(url)
  const handler = routes[path.split('?')[0]] ?? (() => ({}))
  return new Promise((resolve, reject) => {
    signal?.addEventListener('abort', () =>
      reject(new DOMException('aborted', 'AbortError')))
    Promise.resolve(handler(path)).then(body =>
      (body instanceof Response ? resolve(body) : resolve(json(body))))
  })
}

function deferred() {
  let resolve
  const promise = new Promise(r => { resolve = r })
  return { promise, resolve }
}

beforeEach(() => {
  requested = []
  routes = { '/api/v1/cases': () => ({ cases: [lockCase, otherCase] }) }
  vi.stubGlobal('fetch', vi.fn(fakeFetch))
})

afterEach(() => {
  vi.unstubAllGlobals()
  window.history.replaceState(null, '', '#/')
})

const go = hash => window.history.replaceState(null, '', hash)
const titles = () => screen.queryAllByRole('article')
  .map(a => within(a).getByRole('heading', { level: 3 }).textContent)
const detailReads = () => requested.filter(u => u.includes('/investigations/'))

describe('Cases investigation focus over the real useAPI (cases-12)', () => {
  it('shows only the newer investigation after a fast hash change', async () => {
    const slow = deferred()
    routes[detailURL(INV)] = () => slow.promise
    routes[detailURL(INV2)] = () => detail(INV2, otherCase.id)
    go(`#/cases?investigation=${INV}&database=orders`)
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    act(() => {
      go(`#/cases?investigation=${INV2}&database=orders`)
      window.dispatchEvent(new Event('hashchange'))
    })
    expect(await screen.findByText('Missing index')).toBeInTheDocument()
    await act(async () => { slow.resolve(detail(INV, lockCase.id)) })
    expect(titles()).toEqual(['Missing index'])
    expect(detailReads()).toEqual([detailURL(INV), detailURL(INV2)])
  })

  it('shows a 503 with Retry, and Retry opens the case', async () => {
    let calls = 0
    routes[detailURL(INV)] = () => (++calls === 1
      ? json({ error: 'investigation store unavailable', code: 'metadata_unavailable' },
        503)
      : detail(INV, lockCase.id))
    go(`#/cases?investigation=${INV}&database=orders`)
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    expect(await screen.findByRole('alert'))
      .toHaveTextContent('investigation store unavailable (503)')
    expect(screen.queryByTestId('cases-focus-missing')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('Lock storm')).toBeInTheDocument()
    expect(titles()).toEqual(['Lock storm'])
    expect(detailReads()).toEqual([detailURL(INV), detailURL(INV)])
  })

  it('links an Ask Sage investigation to the record of the database it was read from',
    async () => {
      // The server's investigation carries no database (sre.Investigation);
      // the record link takes it from the detail's top-level database.
      routes[detailURL(INV)] = () => detail(INV, 'ask:0123456789abcdef')
      go(`#/cases?investigation=${INV}&database=orders`)
      render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
      const notice = await screen.findByTestId('cases-focus-missing')
      expect(notice).toHaveTextContent(/no case here/)
      expect(within(notice).getByRole('link', { name: /investigation record/i }))
        .toHaveAttribute('href', detailURL(INV))
      expect(requested.some(u => u.startsWith('/api/v1/cases') && u.includes('case=')))
        .toBe(false)
    })

  it('says not found for a 404 and reads the investigation once', async () => {
    routes[detailURL(INV)] = () =>
      json({ error: 'investigation not found', code: 'not_found' }, 404)
    go(`#/cases?investigation=${INV}&database=orders`)
    render(<CasesPage database="orders" user={{ role: 'viewer' }} />)
    expect(await screen.findByTestId('cases-focus-missing'))
      .toHaveTextContent(`No case is loaded for investigation ${INV}`)
    expect(screen.queryByRole('alert')).toBeNull()
    expect(detailReads()).toEqual([detailURL(INV)])
    expect(requested.some(u => u.startsWith('/api/v1/investigations?') &&
      !u.includes('case_id='))).toBe(false)
  })
})
