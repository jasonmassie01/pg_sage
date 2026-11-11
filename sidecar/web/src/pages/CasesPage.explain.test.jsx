import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

// tuning-10: a query case's evidence opens the Explain panel for the
// evidence's exact query id, on the case's own database.

const queryCase = (evidence) => ({
  case_id: 'q1', source_type: 'finding', database_name: 'orders db', state: 'open',
  title: 'A query is taking longer than the configured threshold', severity: 'warning',
  identity_key: 'finding:orders db:slow_query:query:select', evidence: [evidence],
})

let cases = []
vi.mock('../hooks/useAPI', () => ({
  useAPI: url => {
    if (url?.startsWith('/api/v1/investigations')) {
      return { data: { items: [] }, loading: false, error: null, refetch: vi.fn() }
    }
    if (url?.startsWith('/api/v1/facts/match')) {
      return { data: { objects: {} }, loading: false, error: null, refetch: vi.fn() }
    }
    return { data: { cases }, loading: false, error: null, refetch: vi.fn() }
  },
}))

describe('CasesPage explain', () => {
  const realFetch = globalThis.fetch
  afterEach(() => { globalThis.fetch = realFetch })

  it('opens the Explain panel from a query case', async () => {
    const calls = []
    globalThis.fetch = vi.fn(async (url, init) => {
      calls.push({ url, init })
      const body = { summary: 'Sort; sort spilled 8.2 MB to disk',
        slow_because: ['sort spilled 8.2 MB to disk'], recommendations: [],
        insights: [{ kind: 'slow_because', text: 'sort spilled 8.2 MB to disk',
          source: 'rules' }], node_breakdown: [], plan_source: 'explain', plan_age_s: 0 }
      return { ok: true, status: 200, text: async () => JSON.stringify(body) }
    })
    cases = [queryCase({ type: 'finding', summary: 'slow query', queryid: '-5191732810777595558',
      detail: { query: 'SELECT payload FROM orders ORDER BY payload',
        queryid: Number('-5191732810777595558') } })]
    render(<CasesPage database="all" user={{ role: 'operator' }} />)
    fireEvent.click(screen.getByRole('button', { name: 'Explain this query' }))
    await waitFor(() => expect(screen.getByText('sort spilled 8.2 MB to disk'))
      .toBeInTheDocument())
    expect(calls[0].url).toBe('/api/v1/explain?database=orders%20db')
    expect(JSON.parse(calls[0].init.body)).toEqual({ query_id: '-5191732810777595558' })
  })

  it('offers a viewer no Explain button (the endpoint needs operator)', () => {
    cases = [queryCase({ type: 'finding', summary: 'slow query', queryid: '77',
      detail: { query: 'SELECT 1', queryid: 77 } })]
    render(<CasesPage database="all" user={{ role: 'viewer' }} />)
    expect(screen.queryByRole('button', { name: 'Explain this query' })).not.toBeInTheDocument()
  })

  it('offers an admin the Explain button', () => {
    cases = [queryCase({ type: 'finding', summary: 'slow query', queryid: '77',
      detail: { query: 'SELECT 1', queryid: 77 } })]
    render(<CasesPage database="all" user={{ role: 'admin' }} />)
    expect(screen.getByRole('button', { name: 'Explain this query' })).toBeInTheDocument()
  })

  it('offers no Explain button when the evidence has no exact query id', () => {
    cases = [queryCase({ type: 'finding', summary: 'slow query',
      detail: { query: 'SELECT 1', queryid: Number('-5191732810777595558') } })]
    render(<CasesPage database="all" user={{ role: 'operator' }} />)
    expect(screen.queryByRole('button', { name: 'Explain this query' })).not.toBeInTheDocument()
  })
})
