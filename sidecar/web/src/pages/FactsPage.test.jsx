import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { FactsPage } from './FactsPage'

// The Facts page (roadmap 2.3) lists a database's binding facts: what
// pg_sage proposed or an operator declared, with the evidence behind
// each, and lets operators and admins confirm, reject or expire them.
// fetch is stubbed (the real useAPI runs), so the GET and POST calls the
// page makes are asserted directly.

const proposed = {
  id: 12, database: 'orders', type: 'owned_by_app_migrations', subject_kind: 'index',
  subject: 'public.idx_orders_status', value: {}, source: 'model',
  proposed_by: 'model:advisor',
  evidence: [{ kind: 'migration_file', ref: 'db/migrate/20240101_add_status_idx.rb',
    detail: 'creates idx_orders_status', observed_at: '2026-10-03T10:00:00Z' }],
  rationale: 'The index name matches a Rails migration in the app repo',
  status: 'proposed', decided_by: '', decided_at: null, decision_note: '',
  expires_at: null, expired_reason: '', proposals: 3,
  created_at: '2026-10-01T10:00:00Z', updated_at: '2026-10-03T10:00:00Z',
  summary: "public.idx_orders_status is owned by the application's migrations",
  provenance: 'fact #12, proposed by model:advisor on 2026-10-03',
}

const confirmed = {
  ...proposed, id: 7, type: 'table_window', subject_kind: 'table',
  subject: 'public.events', value: { kind: 'maintenance', window: 'weekends' },
  source: 'operator', proposed_by: 'alice@example.com', evidence: [],
  rationale: '', status: 'confirmed', decided_by: 'alice@example.com',
  decided_at: '2026-10-04T09:00:00Z', decision_note: 'agreed with the app team',
  expires_at: '2026-12-31T00:00:00Z', proposals: 1,
  summary: 'public.events has a maintenance window: weekends',
  provenance: 'fact #7, confirmed by alice@example.com on 2026-10-04',
}

const expired = {
  ...proposed, id: 3, type: 'slot_consumer', subject_kind: 'slot',
  subject: 'debezium_orders', value: { consumer: 'debezium' }, status: 'expired',
  expired_reason: 'the slot no longer exists', proposals: 1,
  summary: 'slot debezium_orders is consumed by debezium',
  provenance: 'fact #3, expired on 2026-10-04',
}

function response(status, body) {
  return { ok: status >= 200 && status < 300, status, statusText: `status ${status}`,
    json: async () => body }
}

// stubFetch answers GET /api/v1/facts with listBody(url) and any POST with
// postReply(url, init).
function stubFetch({ listBody, postReply }) {
  const mock = vi.fn(async (url, init = {}) => {
    if ((init.method || 'GET') === 'GET') return response(200, listBody(String(url)))
    return postReply(String(url), init)
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

const gets = mock => mock.mock.calls.filter(([, init]) => !init?.method
  || init.method === 'GET').map(([url]) => String(url))
const posts = mock => mock.mock.calls.filter(([, init]) => init?.method === 'POST')

const operator = { role: 'operator', email: 'bob@example.com' }

afterEach(() => vi.unstubAllGlobals())

describe('FactsPage listing', () => {
  it('explains facts and lists proposed facts with evidence and provenance', async () => {
    const mock = stubFetch({ listBody: () => ({ facts: [proposed], total: 1, errors: [] }) })
    render(<FactsPage database="orders" user={operator} />)
    const card = await screen.findByTestId('fact-card-12')
    expect(gets(mock)[0]).toBe('/api/v1/facts?database=orders&status=proposed')
    const explainer = screen.getByTestId('facts-explainer')
    expect(explainer).toHaveTextContent(/only narrow or redirect/i)
    expect(explainer).toHaveTextContent(/source-fix packet/i)
    expect(explainer).toHaveTextContent(/change nothing until a person confirms/i)
    expect(card).toHaveTextContent(proposed.summary)
    expect(within(card).getByTestId('fact-type-12')).toHaveTextContent(
      'owned_by_app_migrations')
    expect(within(card).getByTestId('fact-status-12')).toHaveTextContent('proposed')
    expect(card).toHaveTextContent('index public.idx_orders_status')
    expect(card).toHaveTextContent('model')
    expect(card).toHaveTextContent('model:advisor')
    const evidence = within(card).getByTestId('fact-evidence-12')
    expect(evidence).toHaveTextContent('migration_file')
    expect(evidence).toHaveTextContent('db/migrate/20240101_add_status_idx.rb')
    expect(evidence).toHaveTextContent('creates idx_orders_status')
    expect(card).toHaveTextContent(proposed.rationale)
    expect(within(card).getByTestId('fact-provenance-12')).toHaveTextContent(
      'fact #12, proposed by model:advisor on 2026-10-03')
    expect(within(card).getByTestId('fact-proposals-12')).toHaveTextContent('3')
  })

  it('switches between Proposed, Confirmed and All', async () => {
    const mock = stubFetch({ listBody: url => ({
      facts: url.includes('status=confirmed') ? [confirmed]
        : url.includes('status=proposed') ? [proposed] : [proposed, confirmed, expired],
      total: 1, errors: [] }) })
    render(<FactsPage database="orders" user={operator} />)
    await screen.findByTestId('fact-card-12')
    expect(screen.getByTestId('facts-tab-proposed')).toHaveAttribute('aria-pressed', 'true')

    fireEvent.click(screen.getByTestId('facts-tab-confirmed'))
    const card = await screen.findByTestId('fact-card-7')
    expect(gets(mock)).toContain('/api/v1/facts?database=orders&status=confirmed')
    expect(screen.queryByTestId('fact-card-12')).toBeNull()
    expect(card).toHaveTextContent('fact #7, confirmed by alice@example.com on 2026-10-04')
    expect(card).toHaveTextContent('agreed with the app team')
    expect(card).toHaveTextContent('weekends')
    expect(within(card).getByTestId('fact-expires-7')).toHaveTextContent('2026')

    fireEvent.click(screen.getByTestId('facts-tab-all'))
    const old = await screen.findByTestId('fact-card-3')
    // All asks for every status: no status filter at all.
    expect(gets(mock)).toContain('/api/v1/facts?database=orders')
    expect(within(old).getByTestId('fact-status-3')).toHaveTextContent('expired')
    expect(old).toHaveTextContent('the slot no longer exists')
  })

  it('lists every database for "all" and reports per-database errors', async () => {
    const mock = stubFetch({ listBody: () => ({
      facts: [proposed, { ...proposed, id: 40, database: 'billing',
        summary: 'billing fact', provenance: 'fact #40, proposed by model on 2026-10-02' }],
      total: 2, errors: [{ database: 'analytics', error: 'connection refused' }] }) })
    render(<FactsPage database="all" user={operator} />)
    const billing = await screen.findByTestId('fact-card-40')
    expect(gets(mock)[0]).toBe('/api/v1/facts?database=all&status=proposed')
    expect(billing).toHaveTextContent('billing')
    expect(screen.getByTestId('fact-card-12')).toHaveTextContent('orders')
    expect(screen.getByTestId('facts-errors')).toHaveTextContent(
      'analytics: connection refused')
  })

  it('shows an empty state per tab', async () => {
    stubFetch({ listBody: () => ({ facts: [], total: 0, errors: [] }) })
    render(<FactsPage database="orders" user={operator} />)
    expect(await screen.findByTestId('facts-empty')).toHaveTextContent(/no proposed facts/i)
    fireEvent.click(screen.getByTestId('facts-tab-confirmed'))
    await waitFor(() => expect(screen.getByTestId('facts-empty'))
      .toHaveTextContent(/no confirmed facts/i))
  })

  it('shows the error when the list cannot be loaded', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => response(500, { error: 'boom' })))
    render(<FactsPage database="orders" user={operator} />)
    expect(await screen.findByText(/500/)).toBeInTheDocument()
    expect(screen.queryByTestId('fact-card-12')).toBeNull()
  })

  it('tolerates a list response without facts', async () => {
    stubFetch({ listBody: () => ({}) })
    render(<FactsPage database="orders" user={operator} />)
    expect(await screen.findByTestId('facts-empty')).toBeInTheDocument()
  })
})

describe('FactsPage decisions', () => {
  it('confirms a proposed fact with the note and refetches', async () => {
    const mock = stubFetch({ listBody: () => ({ facts: [proposed], errors: [] }),
      postReply: () => response(200, { fact: { ...proposed, status: 'confirmed' } }) })
    render(<FactsPage database="orders" user={operator} />)
    await screen.findByTestId('fact-card-12')
    const before = gets(mock).length
    fireEvent.change(screen.getByTestId('fact-note-12'),
      { target: { value: 'checked the repo' } })
    fireEvent.click(screen.getByTestId('fact-confirm-12'))
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    const [url, init] = posts(mock)[0]
    expect(url).toBe('/api/v1/facts/12/confirm?database=orders')
    expect(init.credentials).toBe('include')
    expect(init.headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(init.body)).toEqual({ note: 'checked the repo' })
    await waitFor(() => expect(gets(mock).length).toBeGreaterThan(before))
  })

  it('rejects a proposed fact with an empty note', async () => {
    const mock = stubFetch({ listBody: () => ({ facts: [proposed], errors: [] }),
      postReply: () => response(200, { fact: { ...proposed, status: 'rejected' } }) })
    render(<FactsPage database="orders" user={operator} />)
    await screen.findByTestId('fact-card-12')
    expect(screen.queryByTestId('fact-expire-12')).toBeNull()
    fireEvent.click(screen.getByTestId('fact-reject-12'))
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    const [url, init] = posts(mock)[0]
    expect(url).toBe('/api/v1/facts/12/reject?database=orders')
    expect(JSON.parse(init.body)).toEqual({ note: '' })
  })

  it('revokes and expires confirmed facts', async () => {
    const mock = stubFetch({ listBody: () => ({ facts: [confirmed], errors: [] }),
      postReply: () => response(200, { fact: confirmed }) })
    render(<FactsPage database="orders" user={{ role: 'admin' }} />)
    fireEvent.click(screen.getByTestId('facts-tab-confirmed'))
    await screen.findByTestId('fact-card-7')
    expect(screen.queryByTestId('fact-confirm-7')).toBeNull()
    fireEvent.change(screen.getByTestId('fact-note-7'), { target: { value: 'app moved' } })
    fireEvent.click(screen.getByTestId('fact-revoke-7'))
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    expect(posts(mock)[0][0]).toBe('/api/v1/facts/7/reject?database=orders')
    expect(JSON.parse(posts(mock)[0][1].body)).toEqual({ note: 'app moved' })
    // The card's buttons stay disabled until the revoke request settles; a
    // click before then is ignored (CI: only one POST was sent).
    await waitFor(() => expect(screen.getByTestId('fact-expire-7').disabled).toBe(false))
    fireEvent.click(screen.getByTestId('fact-expire-7'))
    await waitFor(() => expect(posts(mock)).toHaveLength(2))
    expect(posts(mock)[1][0]).toBe('/api/v1/facts/7/expire?database=orders')
  })

  it("decides a fact on its own database when listing all", async () => {
    const billing = { ...proposed, id: 40, database: 'billing' }
    const mock = stubFetch({ listBody: () => ({ facts: [billing], errors: [] }),
      postReply: () => response(200, { fact: billing }) })
    render(<FactsPage database="all" user={operator} />)
    await screen.findByTestId('fact-card-40')
    fireEvent.click(screen.getByTestId('fact-confirm-40'))
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    expect(posts(mock)[0][0]).toBe('/api/v1/facts/40/confirm?database=billing')
  })

  it('shows the API error when a decision is refused', async () => {
    stubFetch({ listBody: () => ({ facts: [proposed], errors: [] }),
      postReply: () => response(409, { error: 'fact #12 is already confirmed',
        code: 'invalid_transition' }) })
    render(<FactsPage database="orders" user={operator} />)
    await screen.findByTestId('fact-card-12')
    fireEvent.click(screen.getByTestId('fact-confirm-12'))
    expect(await screen.findByTestId('fact-error-12')).toHaveTextContent(
      'fact #12 is already confirmed')
  })

  it('explains a content_changed refusal', async () => {
    stubFetch({ listBody: () => ({ facts: [proposed], errors: [] }),
      postReply: () => response(409, { error: 'changed', code: 'content_changed' }) })
    render(<FactsPage database="orders" user={operator} />)
    await screen.findByTestId('fact-card-12')
    fireEvent.click(screen.getByTestId('fact-confirm-12'))
    expect(await screen.findByTestId('fact-error-12')).toHaveTextContent(
      /changed since it was shown/i)
  })

  it('shows a network failure on a decision', async () => {
    stubFetch({ listBody: () => ({ facts: [proposed], errors: [] }),
      postReply: () => { throw new TypeError('network down') } })
    render(<FactsPage database="orders" user={operator} />)
    await screen.findByTestId('fact-card-12')
    fireEvent.click(screen.getByTestId('fact-reject-12'))
    expect(await screen.findByTestId('fact-error-12')).toHaveTextContent('network down')
  })

  it('gives a viewer no decision controls and no declare form', async () => {
    stubFetch({ listBody: () => ({ facts: [proposed, confirmed], errors: [] }) })
    render(<FactsPage database="orders" user={{ role: 'viewer' }} />)
    const card = await screen.findByTestId('fact-card-12')
    expect(within(card).getByTestId('fact-evidence-12')).toBeInTheDocument()
    for (const id of ['fact-confirm-12', 'fact-reject-12', 'fact-note-12',
      'fact-revoke-7', 'fact-expire-7', 'declare-fact-form']) {
      expect(screen.queryByTestId(id)).toBeNull()
    }
  })
})
