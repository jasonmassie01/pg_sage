import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { FactBadges } from './FactBadges'

// FactBadges shows, next to a finding or an approval card, the binding
// facts about its objects: confirmed facts as "Bound by fact #N" chips and
// proposed ones with Confirm / Reject for operators and admins.

const bound = {
  id: 12, database: 'orders', type: 'owned_by_app_migrations', status: 'confirmed',
  subject_kind: 'table', subject: 'public.orders',
  summary: "public.orders is owned by the application's migrations",
  provenance: 'fact #12, confirmed by alice@example.com on 2026-10-04',
}
const pending = {
  id: 13, database: 'orders', type: 'slot_consumer', status: 'proposed',
  subject_kind: 'slot', subject: 'cdc', summary: 'slot cdc is consumed by debezium',
  provenance: 'fact #13, proposed by model:advisor on 2026-10-03',
}

function response(status, body) {
  return { ok: status >= 200 && status < 300, status, statusText: `status ${status}`,
    json: async () => body }
}

function matchBody(objects) {
  return { database: 'orders', objects }
}

function stubFetch(getBody, postReply = () => response(200, { fact: pending })) {
  const mock = vi.fn(async (url, init = {}) => {
    if ((init.method || 'GET') === 'GET') return response(200, getBody(String(url)))
    return postReply(String(url), init)
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

const gets = mock => mock.mock.calls.filter(([, init]) => !init?.method
  || init.method === 'GET').map(([url]) => String(url))
const posts = mock => mock.mock.calls.filter(([, init]) => init?.method === 'POST')
const flush = () => act(async () => { await new Promise(r => setTimeout(r, 0)) })

afterEach(() => vi.unstubAllGlobals())

describe('FactBadges', () => {
  it('asks about every object and shows a bound chip with provenance', async () => {
    const mock = stubFetch(() => matchBody({
      'public.orders': { confirmed: [bound], proposed: [] },
      'slot:cdc': { confirmed: [], proposed: [] } }))
    render(<FactBadges database="orders" objects={['public.orders', 'slot:cdc']} />)
    const chip = await screen.findByTestId('fact-bound-12')
    expect(gets(mock)[0]).toBe(
      '/api/v1/facts/match?database=orders&object=public.orders&object=slot%3Acdc')
    expect(chip).toHaveTextContent(
      "Bound by fact #12: public.orders is owned by the application's migrations " +
      '(fact #12, confirmed by alice@example.com on 2026-10-04)')
  })

  it('shows a fact matched by two objects once', async () => {
    stubFetch(() => matchBody({
      'public.orders': { confirmed: [bound], proposed: [] },
      'public.orders_pkey': { confirmed: [bound], proposed: [] } }))
    render(<FactBadges database="orders"
      objects={['public.orders', 'public.orders_pkey', 'public.orders']} />)
    await screen.findByTestId('fact-bound-12')
    expect(screen.getAllByTestId('fact-bound-12')).toHaveLength(1)
  })

  it('offers Confirm and Reject on proposed facts to operators', async () => {
    const onChanged = vi.fn()
    const mock = stubFetch(() => matchBody({
      'slot:cdc': { confirmed: [], proposed: [pending] } }))
    render(<FactBadges database="orders" objects={['slot:cdc']} canDecide
      onChanged={onChanged} />)
    const chip = await screen.findByTestId('fact-proposed-13')
    expect(chip).toHaveTextContent('Proposed fact #13: slot cdc is consumed by debezium')
    fireEvent.click(screen.getByTestId('fact-badge-confirm-13'))
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    const [url, init] = posts(mock)[0]
    expect(url).toBe('/api/v1/facts/13/confirm?database=orders')
    expect(init.credentials).toBe('include')
    expect(init.headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(init.body)).toEqual({ note: '' })
    await waitFor(() => expect(gets(mock).length).toBe(2))
    expect(onChanged).toHaveBeenCalledTimes(1)
  })

  it('rejects a proposed fact', async () => {
    const mock = stubFetch(() => matchBody({
      'slot:cdc': { confirmed: [], proposed: [pending] } }))
    render(<FactBadges database="orders" objects={['slot:cdc']} canDecide />)
    await screen.findByTestId('fact-proposed-13')
    fireEvent.click(screen.getByTestId('fact-badge-reject-13'))
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    expect(posts(mock)[0][0]).toBe('/api/v1/facts/13/reject?database=orders')
  })

  it('gives viewers the proposed fact without buttons', async () => {
    stubFetch(() => matchBody({
      'slot:cdc': { confirmed: [bound], proposed: [pending] } }))
    render(<FactBadges database="orders" objects={['slot:cdc']} />)
    await screen.findByTestId('fact-proposed-13')
    expect(screen.getByTestId('fact-bound-12')).toBeInTheDocument()
    expect(screen.queryByTestId('fact-badge-confirm-13')).toBeNull()
    expect(screen.queryByTestId('fact-badge-reject-13')).toBeNull()
  })

  it('shows a refused decision', async () => {
    stubFetch(() => matchBody({ 'slot:cdc': { confirmed: [], proposed: [pending] } }),
      () => response(409, { error: 'fact #13 is already rejected',
        code: 'invalid_transition' }))
    render(<FactBadges database="orders" objects={['slot:cdc']} canDecide />)
    await screen.findByTestId('fact-proposed-13')
    fireEvent.click(screen.getByTestId('fact-badge-confirm-13'))
    expect(await screen.findByTestId('fact-badges-action-error')).toHaveTextContent(
      'fact #13 is already rejected')
  })

  it('renders nothing when no fact matches', async () => {
    const mock = stubFetch(() => matchBody({
      'public.orders': { confirmed: [], proposed: [] } }))
    const { container } = render(
      <FactBadges database="orders" objects={['public.orders']} canDecide />)
    await waitFor(() => expect(gets(mock)).toHaveLength(1))
    await flush()
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing for a malformed match response', async () => {
    const mock = stubFetch(() => ({ database: 'orders', objects: { 'public.orders': null } }))
    const { container } = render(<FactBadges database="orders" objects={['public.orders']} />)
    await waitFor(() => expect(gets(mock)).toHaveLength(1))
    await flush()
    expect(container).toBeEmptyDOMElement()
  })

  it('asks nothing without one database or without objects', async () => {
    const mock = stubFetch(() => matchBody({}))
    const cases = [
      { database: 'all', objects: ['public.orders'] },
      { database: '', objects: ['public.orders'] },
      { database: undefined, objects: ['public.orders'] },
      { database: 'orders', objects: [] },
      { database: 'orders', objects: undefined },
      { database: 'orders', objects: ['', null] },
    ]
    for (const props of cases) {
      const { container, unmount } = render(<FactBadges {...props} />)
      await flush()
      expect(container).toBeEmptyDOMElement()
      unmount()
    }
    expect(mock).not.toHaveBeenCalled()
  })

  it('survives a failed match request', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => response(500, { error: 'boom' })))
    render(<FactBadges database="orders" objects={['public.orders']} canDecide />)
    expect(await screen.findByTestId('fact-badges-error')).toHaveTextContent(/500/)
    expect(screen.queryByTestId('fact-bound-12')).toBeNull()
  })

  it('survives a network failure', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('offline') }))
    render(<FactBadges database="orders" objects={['public.orders']} />)
    expect(await screen.findByTestId('fact-badges-error')).toHaveTextContent('offline')
  })
})
