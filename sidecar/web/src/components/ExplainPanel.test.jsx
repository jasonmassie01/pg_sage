import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ExplainPanel, ExplainQueryButton } from './ExplainPanel'
import { exactQueryID, planSourceLabel } from '../lib/explain'

// tuning-10: the Explain panel shows the deterministic rule entries (and
// the model's, when an LLM is configured) labelled by source, the node
// table, and where the plan came from.

const rulesOnly = {
  query: 'SELECT payload FROM orders ORDER BY payload',
  summary: 'Sort; sort spilled 8.2 MB to disk',
  slow_because: ['sort spilled 8.2 MB to disk'],
  recommendations: ['raise work_mem for this statement'],
  insights: [
    { kind: 'slow_because', text: 'sort spilled 8.2 MB to disk', source: 'rules',
      symptom: 'disk_sort' },
    { kind: 'recommendation', text: 'raise work_mem for this statement', source: 'rules',
      symptom: 'disk_sort' },
  ],
  node_breakdown: [
    { node_type: 'Sort', description: 'Sort', time_ms: 30.5, loops: 1, rows: 1000,
      row_estimate: 1000 },
    { node_type: 'Index Scan', relation: 'customers', description: 'Index Scan on customers',
      time_ms: 1000, loops: 5000, rows: 0, row_estimate: 50,
      warning: 'row estimate off by 50x (est 50, actual 0)' },
  ],
  estimated_cost: 512.5,
  actual_time_ms: 41.5,
  plan_source: 'explain',
  plan_age_s: 0,
}

const withModel = {
  ...rulesOnly,
  summary: 'The query sorts every order by payload.',
  slow_because: [...rulesOnly.slow_because, 'the sort reads the whole table'],
  insights: [...rulesOnly.insights,
    { kind: 'slow_because', text: 'the sort reads the whole table', source: 'llm' }],
  plan_source: 'explain_cache',
  plan_capture: 'auto_explain',
  plan_age_s: 150,
}

let calls = []
function respond(status, body) {
  globalThis.fetch = vi.fn(async (url, init) => {
    calls.push({ url, init })
    return {
      ok: status >= 200 && status < 300,
      status,
      statusText: '',
      text: async () => JSON.stringify(body),
      json: async () => body,
    }
  })
}

describe('ExplainPanel', () => {
  const realFetch = globalThis.fetch
  beforeEach(() => { calls = [] })
  afterEach(() => { globalThis.fetch = realFetch })

  it('renders rule entries with a rules badge and works with no LLM', async () => {
    respond(200, rulesOnly)
    render(<ExplainPanel database="orders_db" queryid="-5191732810777595558" />)
    const reasons = await screen.findByTestId('explain-slow-because')
    const entry = within(reasons).getByText('sort spilled 8.2 MB to disk').closest('li')
    expect(within(entry).getByText('rules')).toBeInTheDocument()
    expect(screen.getByText('Sort; sort spilled 8.2 MB to disk')).toBeInTheDocument()
    const recs = screen.getByTestId('explain-recommendations')
    expect(within(recs).getByText('raise work_mem for this statement')).toBeInTheDocument()
    expect(screen.queryByText('model')).not.toBeInTheDocument()
  })

  it('posts the exact query id as a string to the database it names', async () => {
    respond(200, rulesOnly)
    render(<ExplainPanel database="orders db" queryid="-5191732810777595558" />)
    await screen.findByTestId('explain-slow-because')
    expect(calls).toHaveLength(1)
    expect(calls[0].url).toBe('/api/v1/explain?database=orders%20db')
    expect(calls[0].init.method).toBe('POST')
    expect(JSON.parse(calls[0].init.body)).toEqual({ query_id: '-5191732810777595558' })
  })

  it('labels model entries apart from rule entries', async () => {
    respond(200, withModel)
    render(<ExplainPanel database="db" queryid="42" />)
    const reasons = await screen.findByTestId('explain-slow-because')
    const model = within(reasons).getByText('the sort reads the whole table').closest('li')
    expect(within(model).getByText('model')).toBeInTheDocument()
    const rule = within(reasons).getByText('sort spilled 8.2 MB to disk').closest('li')
    expect(within(rule).getByText('rules')).toBeInTheDocument()
  })

  it('shows the plan source and age', async () => {
    respond(200, withModel)
    render(<ExplainPanel database="db" queryid="42" />)
    expect(await screen.findByTestId('explain-plan-source'))
      .toHaveTextContent('captured by auto_explain, 2m old')
  })

  it('shows the node table with inclusive time, loops and the warning', async () => {
    respond(200, rulesOnly)
    render(<ExplainPanel database="db" queryid="42" />)
    const table = await screen.findByTestId('explain-nodes')
    const row = within(table).getByText('Index Scan on customers').closest('tr')
    expect(within(row).getByText('1000.00 ms')).toBeInTheDocument()
    expect(within(row).getByText('5000')).toBeInTheDocument()
    expect(within(row).getByText('row estimate off by 50x (est 50, actual 0)'))
      .toBeInTheDocument()
  })

  it('says so when the rules found nothing', async () => {
    respond(200, { ...rulesOnly, slow_because: [], recommendations: [], insights: [],
      summary: 'Index Scan on orders; the plan rules found no problem' })
    render(<ExplainPanel database="db" queryid="42" />)
    expect(await screen.findByText(/the plan rules found no problem/)).toBeInTheDocument()
    expect(screen.getByText('No reasons found.')).toBeInTheDocument()
  })

  it('shows the server message on a 400', async () => {
    respond(400, { error: 'invalid explain request: no pg_stat_statements entry 42' })
    render(<ExplainPanel database="db" queryid="42" />)
    expect(await screen.findByRole('alert'))
      .toHaveTextContent('no pg_stat_statements entry 42 (400)')
  })

  // dashboard-13: a failed explain could only be retried by closing and
  // reopening the panel.
  it('offers Retry after a 500 and shows the result of the new request', async () => {
    respond(500, { error: 'explain failed: canceling statement due to timeout' })
    render(<ExplainPanel database="orders" queryid="42" />)
    expect(await screen.findByRole('alert'))
      .toHaveTextContent('explain failed: canceling statement due to timeout (500)')
    respond(200, rulesOnly)
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByTestId('explain-slow-because'))
      .toHaveTextContent('sort spilled 8.2 MB to disk')
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull()
    expect(calls).toHaveLength(2)
    expect(JSON.parse(calls[1].init.body)).toEqual({ query_id: '42' })
    expect(calls[1].url).toBe('/api/v1/explain?database=orders')
  })

  it('offers no Retry while the result is shown', async () => {
    respond(200, rulesOnly)
    render(<ExplainPanel database="orders" queryid="42" />)
    await screen.findByTestId('explain-slow-because')
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull()
  })

  it('tolerates a response without insights (an older cached result)', async () => {
    respond(200, { ...rulesOnly, insights: undefined })
    render(<ExplainPanel database="db" queryid="42" />)
    const reasons = await screen.findByTestId('explain-slow-because')
    expect(within(reasons).getByText('sort spilled 8.2 MB to disk')).toBeInTheDocument()
  })
})

describe('ExplainQueryButton', () => {
  const realFetch = globalThis.fetch
  afterEach(() => { globalThis.fetch = realFetch })

  it('opens the panel on click and closes it again', async () => {
    respond(200, rulesOnly)
    render(<ExplainQueryButton database="db" queryid="42" />)
    expect(screen.queryByTestId('explain-panel')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Explain this query' }))
    await waitFor(() => expect(screen.getByTestId('explain-panel')).toBeInTheDocument())
    fireEvent.click(screen.getByRole('button', { name: 'Close explain' }))
    expect(screen.queryByTestId('explain-panel')).not.toBeInTheDocument()
  })

  it('renders nothing without an exact query id', () => {
    const { container } = render(<ExplainQueryButton database="db" queryid="" />)
    expect(container).toBeEmptyDOMElement()
  })
})

describe('exactQueryID', () => {
  it('keeps decimal strings and safe integers, drops anything rounded or odd', () => {
    expect(exactQueryID('-5191732810777595558')).toBe('-5191732810777595558')
    expect(exactQueryID(42)).toBe('42')
    expect(exactQueryID(Number('-5191732810777595558'))).toBe('')
    expect(exactQueryID(42.5)).toBe('')
    expect(exactQueryID('42; DROP TABLE x')).toBe('')
    expect(exactQueryID('0042')).toBe('')
    expect(exactQueryID('-0')).toBe('')
    expect(exactQueryID(null)).toBe('')
    expect(exactQueryID(undefined)).toBe('')
  })
})

describe('planSourceLabel', () => {
  it('names a fresh plan and a captured one', () => {
    expect(planSourceLabel({ plan_source: 'explain', actual_time_ms: 2 }))
      .toBe('fresh EXPLAIN ANALYZE')
    expect(planSourceLabel({ plan_source: 'explain' })).toBe('fresh EXPLAIN')
    expect(planSourceLabel({ plan_source: 'explain_cache', plan_capture: 'auto_explain',
      plan_age_s: 30 })).toBe('captured by auto_explain, 30s old')
    expect(planSourceLabel({})).toBe('')
  })
})
