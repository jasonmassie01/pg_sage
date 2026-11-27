import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { OwnerBriefForm } from './OwnerBriefForm'

// evid-03b: the owner brief form shows the database's current brief and a
// pending peak-window suggestion (GET /api/v1/databases/{db}/facts/
// peak-suggestion). Applying the suggestion fills the peak windows field;
// saving is an ordinary declaration of the brief (POST /api/v1/facts),
// which replaces the whole value, so fields the form does not edit
// (growth_pct_month) are carried over. No concurrency tests: one person
// edits one form.

const BRIEF_URL = '/api/v1/facts?database=orders&status=confirmed&type=owner_brief'
const PEAK_URL = '/api/v1/databases/orders/facts/peak-suggestion'

function response(status, body) {
  return { ok: status >= 200 && status < 300, status, json: async () => body }
}

const brief = {
  id: 5, database: 'orders', type: 'owner_brief', subject_kind: 'database', subject: '*',
  value: { owner: 'Payments', contact: '#pay', peak_windows: 'weekdays 09:00-17:00',
    growth_pct_month: '5' },
  status: 'confirmed', summary: 'owner brief: owner Payments, contact #pay',
  provenance: 'fact #5, confirmed by alice@example.com on 2026-11-01',
}

const suggestion = {
  database: 'orders', suggestion: 'peak_windows', peak_windows: 'mon 09:00-12:00 UTC',
  coverage_status: 'ok', hours: [{ hour: '2026-10-19T09:00:00Z', exec_ms: 10000 },
    { hour: '2026-10-26T09:00:00Z', exec_ms: 9000 }],
}

// stub answers each URL from routes (a response or a function of the
// request init); unknown URLs fail the test.
function stub(routes) {
  const mock = vi.fn(async (url, init) => {
    const r = routes[url]
    if (!r) throw new Error(`unexpected fetch ${url}`)
    return typeof r === 'function' ? r(init) : r
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

afterEach(() => vi.unstubAllGlobals())

describe('OwnerBriefForm', () => {
  it('shows the brief and a pending suggestion; apply then save declares it', async () => {
    const posted = []
    const mock = stub({
      [BRIEF_URL]: response(200, { facts: [brief], errors: [] }),
      [PEAK_URL]: response(200, suggestion),
      '/api/v1/facts?database=orders': init => {
        posted.push(JSON.parse(init.body))
        return response(201, { fact: { ...brief, id: 5, summary: 'owner brief: saved' } })
      },
    })
    const onDeclared = vi.fn()
    render(<OwnerBriefForm database="orders" canDecide onDeclared={onDeclared} />)
    expect(await screen.findByTestId('brief-owner')).toHaveValue('Payments')
    expect(screen.getByTestId('brief-peak-windows')).toHaveValue('weekdays 09:00-17:00')
    const pending = await screen.findByTestId('brief-suggestion')
    expect(pending).toHaveTextContent('mon 09:00-12:00 UTC')
    expect(pending).toHaveTextContent('2 rollup hours')
    fireEvent.click(screen.getByTestId('brief-suggestion-apply'))
    expect(screen.getByTestId('brief-peak-windows')).toHaveValue('mon 09:00-12:00 UTC')
    fireEvent.change(screen.getByTestId('brief-retention'), { target: { value: '7 years' } })
    fireEvent.click(screen.getByTestId('brief-save'))
    expect(await screen.findByTestId('brief-success')).toHaveTextContent('owner brief: saved')
    expect(posted).toEqual([{ type: 'owner_brief', subject_kind: 'database', subject: '*',
      value: { owner: 'Payments', contact: '#pay', peak_windows: 'mon 09:00-12:00 UTC',
        growth_pct_month: '5', retention: '7 years' }, note: '' }])
    expect(onDeclared).toHaveBeenCalledTimes(1)
    const post = mock.mock.calls.find(c => c[1]?.method === 'POST')
    expect(post[1].credentials).toBe('include')
  })

  it('says why there is no suggestion and offers nothing to apply', async () => {
    stub({
      [BRIEF_URL]: response(200, { facts: [], errors: [] }),
      [PEAK_URL]: response(200, { database: 'orders', suggestion: 'none', hours: [],
        reason: 'less than 28 days of hourly rollup', coverage_status: 'learning' }),
    })
    render(<OwnerBriefForm database="orders" canDecide />)
    const none = await screen.findByTestId('brief-suggestion-none')
    expect(none).toHaveTextContent('less than 28 days of hourly rollup')
    expect(screen.queryByTestId('brief-suggestion-apply')).toBeNull()
    expect(screen.getByTestId('brief-owner')).toHaveValue('')
    expect(screen.getByTestId('brief-current')).toHaveTextContent('No owner brief yet')
  })

  it('shows a suggestion that matches the brief as matching, not pending', async () => {
    stub({
      [BRIEF_URL]: response(200, { facts: [{ ...brief,
        value: { peak_windows: 'mon 09:00-12:00 UTC' } }], errors: [] }),
      [PEAK_URL]: response(200, suggestion),
    })
    render(<OwnerBriefForm database="orders" canDecide />)
    expect(await screen.findByTestId('brief-suggestion-match')).toHaveTextContent(
      'match the measured peak')
    expect(screen.queryByTestId('brief-suggestion-apply')).toBeNull()
  })

  it('validates before sending: something to say, bounded fields, no empty windows',
    async () => {
      const mock = stub({
        [BRIEF_URL]: response(200, { facts: [], errors: [] }),
        [PEAK_URL]: response(200, { suggestion: 'none', reason: 'no rollup', hours: [] }),
      })
      render(<OwnerBriefForm database="orders" canDecide />)
      await screen.findByTestId('brief-suggestion-none')
      expect(screen.getByTestId('brief-save')).toBeDisabled()
      fireEvent.change(screen.getByTestId('brief-owner'), { target: { value: 'x'.repeat(301) } })
      expect(screen.getByTestId('brief-invalid')).toHaveTextContent('300')
      expect(screen.getByTestId('brief-save')).toBeDisabled()
      fireEvent.change(screen.getByTestId('brief-owner'), { target: { value: 'Payments' } })
      fireEvent.change(screen.getByTestId('brief-peak-windows'),
        { target: { value: 'weekdays 09:00-17:00;;sat' } })
      expect(screen.getByTestId('brief-invalid')).toHaveTextContent('empty window')
      expect(screen.getByTestId('brief-save')).toBeDisabled()
      fireEvent.change(screen.getByTestId('brief-peak-windows'),
        { target: { value: 'weekdays 09:00-17:00; sat' } })
      expect(screen.queryByTestId('brief-invalid')).toBeNull()
      expect(screen.getByTestId('brief-save')).not.toBeDisabled()
      expect(mock.mock.calls.some(c => c[1]?.method === 'POST')).toBe(false)
    })

  it('shows the server refusal and keeps what was typed', async () => {
    stub({
      [BRIEF_URL]: response(200, { facts: [], errors: [] }),
      [PEAK_URL]: response(200, { suggestion: 'none', reason: 'no rollup', hours: [] }),
      '/api/v1/facts?database=orders': () => response(400, { code: 'invalid_fact',
        error: 'facts: invalid value: peak_windows: "lunch" is not a day list or preset' }),
    })
    render(<OwnerBriefForm database="orders" canDecide />)
    await screen.findByTestId('brief-suggestion-none')
    fireEvent.change(screen.getByTestId('brief-peak-windows'), { target: { value: 'lunch' } })
    fireEvent.click(screen.getByTestId('brief-save'))
    expect(await screen.findByTestId('brief-error')).toHaveTextContent('"lunch" is not a day')
    expect(screen.getByTestId('brief-peak-windows')).toHaveValue('lunch')
    expect(screen.queryByTestId('brief-success')).toBeNull()
  })

  it('reports an unreadable suggestion as an error, never as "none"', async () => {
    stub({
      [BRIEF_URL]: response(200, { facts: [], errors: [] }),
      [PEAK_URL]: response(500, { error: 'read the hourly rollup: connection refused' }),
    })
    render(<OwnerBriefForm database="orders" canDecide />)
    expect(await screen.findByTestId('brief-suggestion-error')).toHaveTextContent(
      'connection refused')
    expect(screen.queryByTestId('brief-suggestion-none')).toBeNull()
    expect(screen.getByTestId('brief-owner')).not.toBeDisabled()
  })

  it('asks for one database and fetches nothing for the fleet view', () => {
    const mock = stub({})
    render(<OwnerBriefForm database="all" canDecide />)
    expect(screen.getByTestId('brief-pick-database')).toBeInTheDocument()
    expect(screen.queryByTestId('brief-owner')).toBeNull()
    expect(mock).not.toHaveBeenCalled()
  })

  it('shows a viewer the brief read-only', async () => {
    stub({
      [BRIEF_URL]: response(200, { facts: [brief], errors: [] }),
      [PEAK_URL]: response(200, suggestion),
    })
    render(<OwnerBriefForm database="orders" canDecide={false} />)
    expect(await screen.findByTestId('brief-current')).toHaveTextContent(
      'owner brief: owner Payments')
    await waitFor(() => expect(screen.getByTestId('brief-suggestion')).toBeInTheDocument())
    expect(screen.queryByTestId('brief-owner')).toBeNull()
    expect(screen.queryByTestId('brief-save')).toBeNull()
    expect(screen.queryByTestId('brief-suggestion-apply')).toBeNull()
  })
})
