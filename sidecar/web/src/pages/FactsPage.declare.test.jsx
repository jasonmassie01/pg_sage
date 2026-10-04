import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { FactsPage } from './FactsPage'

// "Declare a fact": an operator or admin states a fact about one database
// (confirmed by the declaration). The subject kind is limited by the fact
// type, the value fields follow the type, and the API's refusal is shown.

function response(status, body) {
  return { ok: status >= 200 && status < 300, status, statusText: `status ${status}`,
    json: async () => body }
}

const created = {
  id: 21, database: 'orders', type: 'owned_by_app_migrations', subject_kind: 'index',
  subject: 'public.idx_orders_status', value: {}, source: 'operator',
  proposed_by: 'bob@example.com', evidence: [], status: 'confirmed', proposals: 1,
  summary: "public.idx_orders_status is owned by the application's migrations",
  provenance: 'fact #21, confirmed by bob@example.com on 2026-10-04',
}

function stubFetch(postReply = () => response(201, { fact: created })) {
  const mock = vi.fn(async (url, init = {}) => {
    if ((init.method || 'GET') === 'GET') return response(200, { facts: [], errors: [] })
    return postReply(String(url), init)
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

const posts = mock => mock.mock.calls.filter(([, init]) => init?.method === 'POST')
const gets = mock => mock.mock.calls.filter(([, init]) => !init?.method
  || init.method === 'GET')
const options = testId => within(screen.getByTestId(testId)).getAllByRole('option')
  .map(o => o.value)
const choose = (testId, value) => fireEvent.change(screen.getByTestId(testId),
  { target: { value } })

async function renderForm(database = 'orders', role = 'operator') {
  render(<FactsPage database={database} user={{ role }} />)
  return screen.findByTestId('declare-fact-form')
}

afterEach(() => vi.unstubAllGlobals())

describe('Declare a fact form', () => {
  it('offers the five fact types and limits the subject kind by type', async () => {
    stubFetch()
    await renderForm()
    expect(options('declare-type')).toEqual(['owned_by_app_migrations', 'test_fixture',
      'slot_consumer', 'append_only', 'table_window'])
    expect(options('declare-kind')).toEqual(['index', 'table', 'schema'])
    choose('declare-type', 'test_fixture')
    expect(options('declare-kind')).toEqual(['schema'])
    choose('declare-type', 'slot_consumer')
    expect(options('declare-kind')).toEqual(['slot'])
    choose('declare-type', 'append_only')
    expect(options('declare-kind')).toEqual(['table'])
    choose('declare-type', 'table_window')
    expect(options('declare-kind')).toEqual(['table'])
  })

  it('resets the subject kind to one the new type allows', async () => {
    stubFetch()
    await renderForm()
    choose('declare-kind', 'schema')
    expect(screen.getByTestId('declare-kind')).toHaveValue('schema')
    choose('declare-type', 'append_only')
    expect(screen.getByTestId('declare-kind')).toHaveValue('table')
    choose('declare-type', 'slot_consumer')
    expect(screen.getByTestId('declare-kind')).toHaveValue('slot')
  })

  it('shows a subject placeholder for each kind', async () => {
    stubFetch()
    await renderForm()
    const subject = () => screen.getByTestId('declare-subject')
    expect(subject()).toHaveAttribute('placeholder', 'public.idx_orders_status')
    choose('declare-kind', 'table')
    expect(subject()).toHaveAttribute('placeholder', 'public.orders')
    choose('declare-kind', 'schema')
    expect(subject()).toHaveAttribute('placeholder', 'test_*')
    choose('declare-type', 'slot_consumer')
    expect(subject()).toHaveAttribute('placeholder', 'debezium_orders')
  })

  it('shows the value fields the type needs and no others', async () => {
    stubFetch()
    await renderForm()
    const absent = ids => ids.forEach(id => expect(screen.queryByTestId(id)).toBeNull())
    absent(['declare-consumer', 'declare-window-kind', 'declare-window'])
    choose('declare-type', 'slot_consumer')
    expect(screen.getByTestId('declare-consumer')).toBeInTheDocument()
    absent(['declare-window-kind', 'declare-window'])
    choose('declare-type', 'table_window')
    expect(options('declare-window-kind')).toEqual(['maintenance', 'batch'])
    expect(screen.getByTestId('declare-window')).toHaveAttribute('placeholder',
      expect.stringContaining('weekends'))
    absent(['declare-consumer'])
    choose('declare-type', 'append_only')
    absent(['declare-consumer', 'declare-window-kind', 'declare-window'])
  })

  it('posts an owned_by_app_migrations fact and refetches the list', async () => {
    const mock = stubFetch()
    await renderForm()
    const before = gets(mock).length
    choose('declare-subject', 'public.idx_orders_status')
    choose('declare-note', 'from the app repo')
    fireEvent.click(screen.getByTestId('declare-submit'))
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    const [url, init] = posts(mock)[0]
    expect(url).toBe('/api/v1/facts?database=orders')
    expect(init.credentials).toBe('include')
    expect(init.headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(init.body)).toEqual({ type: 'owned_by_app_migrations',
      subject_kind: 'index', subject: 'public.idx_orders_status', value: {},
      note: 'from the app repo' })
    expect(await screen.findByTestId('declare-success')).toHaveTextContent('#21')
    await waitFor(() => expect(gets(mock).length).toBeGreaterThan(before))
    expect(screen.getByTestId('declare-subject')).toHaveValue('')
  })

  it('posts a table_window fact with its kind and window', async () => {
    const mock = stubFetch()
    await renderForm()
    choose('declare-type', 'table_window')
    choose('declare-subject', 'public.events')
    choose('declare-window-kind', 'batch')
    choose('declare-window', 'daily 01:00-03:00 UTC')
    fireEvent.click(screen.getByTestId('declare-submit'))
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    expect(JSON.parse(posts(mock)[0][1].body)).toEqual({ type: 'table_window',
      subject_kind: 'table', subject: 'public.events',
      value: { kind: 'batch', window: 'daily 01:00-03:00 UTC' }, note: '' })
  })

  it('posts a slot_consumer fact with its consumer', async () => {
    const mock = stubFetch()
    await renderForm()
    choose('declare-type', 'slot_consumer')
    choose('declare-subject', '  debezium_orders ')
    choose('declare-consumer', 'debezium')
    fireEvent.click(screen.getByTestId('declare-submit'))
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    expect(JSON.parse(posts(mock)[0][1].body)).toEqual({ type: 'slot_consumer',
      subject_kind: 'slot', subject: 'debezium_orders', value: { consumer: 'debezium' },
      note: '' })
  })

  it('sends expires_at only when one is given', async () => {
    const mock = stubFetch()
    await renderForm()
    choose('declare-type', 'test_fixture')
    choose('declare-subject', 'test_*')
    choose('declare-expires', '2026-12-31T00:00')
    fireEvent.click(screen.getByTestId('declare-submit'))
    await waitFor(() => expect(posts(mock)).toHaveLength(1))
    const body = JSON.parse(posts(mock)[0][1].body)
    expect(body.expires_at).toBe(new Date('2026-12-31T00:00').toISOString())
    expect(body.subject_kind).toBe('schema')
  })

  it("shows the API's error and keeps the form filled", async () => {
    stubFetch(() => response(400, {
      error: 'a subject that could match the sage schema is refused',
      code: 'protected_subject' }))
    await renderForm()
    choose('declare-type', 'test_fixture')
    choose('declare-subject', 's*')
    fireEvent.click(screen.getByTestId('declare-submit'))
    expect(await screen.findByTestId('declare-error')).toHaveTextContent(
      'a subject that could match the sage schema is refused')
    expect(screen.queryByTestId('declare-success')).toBeNull()
    expect(screen.getByTestId('declare-subject')).toHaveValue('s*')
  })

  it('shows a network failure', async () => {
    stubFetch(() => { throw new TypeError('network down') })
    await renderForm()
    choose('declare-subject', 'public.orders')
    fireEvent.click(screen.getByTestId('declare-submit'))
    expect(await screen.findByTestId('declare-error')).toHaveTextContent('network down')
  })

  it('does not post without a subject', async () => {
    const mock = stubFetch()
    await renderForm()
    choose('declare-subject', '   ')
    expect(screen.getByTestId('declare-submit')).toBeDisabled()
    fireEvent.click(screen.getByTestId('declare-submit'))
    expect(posts(mock)).toHaveLength(0)
  })

  it('asks for one database when all are selected', async () => {
    const mock = stubFetch()
    await renderForm('all')
    expect(screen.getByTestId('declare-pick-database')).toBeInTheDocument()
    choose('declare-subject', 'public.orders')
    expect(screen.getByTestId('declare-submit')).toBeDisabled()
    expect(posts(mock)).toHaveLength(0)
  })
})
