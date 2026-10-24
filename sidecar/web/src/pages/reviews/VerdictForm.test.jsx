import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { VerdictForm } from './VerdictForm'
import { admin, makeReview, operator, response, viewer } from './testkit'

// CHECK-UI-02: verdict buttons are hidden for viewers and shown for
// operators and admins. Override keeps Submit disabled until a reason and
// a direction are entered. A 409 shows "already decided by <actor>".

const URL = '/api/v1/databases/orders/change-reviews/' +
  '11111111-1111-4111-8111-111111111111/verdict'

function stubFetch(reply = () => response(200, makeReview({ dba_verdict: 'concur' }))) {
  const mock = vi.fn(async (url, init) => reply(String(url), init))
  vi.stubGlobal('fetch', mock)
  return mock
}

function posted(mock) {
  expect(mock).toHaveBeenCalledTimes(1)
  const [url, init] = mock.mock.calls[0]
  expect(url).toBe(URL)
  expect(init.method).toBe('POST')
  expect(init.headers['Content-Type']).toBe('application/json')
  return JSON.parse(init.body)
}

afterEach(() => vi.unstubAllGlobals())

describe('VerdictForm', () => {
  it('shows no verdict buttons to a viewer', () => {
    const mock = stubFetch()
    render(<VerdictForm review={makeReview()} user={viewer} />)
    expect(screen.queryByTestId('verdict-concur')).toBeNull()
    expect(screen.queryByTestId('verdict-override')).toBeNull()
    expect(screen.queryByTestId('verdict-concur-with-edits')).toBeNull()
    expect(screen.getByTestId('verdict-viewer-note'))
      .toHaveTextContent(/operators and admins/i)
    expect(mock).not.toHaveBeenCalled()
  })

  it.each([['operator', operator], ['admin', admin]])(
    'shows the three verdicts to an %s', (_, user) => {
      stubFetch()
      render(<VerdictForm review={makeReview()} user={user} />)
      expect(screen.getByTestId('verdict-concur')).toBeEnabled()
      expect(screen.getByTestId('verdict-concur-with-edits')).toBeEnabled()
      expect(screen.getByTestId('verdict-override')).toBeEnabled()
      expect(screen.queryByTestId('verdict-viewer-note')).toBeNull()
    })

  it('records a concur and reports the new review', async () => {
    const decided = makeReview({ dba_verdict: 'concur', dba_actor: 'user:2',
      credit_state: 'pending', credited_minutes: 15 })
    const mock = stubFetch(() => response(200, decided))
    const onDecided = vi.fn()
    render(<VerdictForm review={makeReview()} user={operator} onDecided={onDecided} />)
    fireEvent.click(screen.getByTestId('verdict-concur'))
    expect(screen.getByTestId('verdict-submit')).toBeEnabled()
    fireEvent.click(screen.getByTestId('verdict-submit'))
    await waitFor(() => expect(onDecided).toHaveBeenCalledWith(decided))
    expect(posted(mock)).toEqual({ verdict: 'concur' })
  })

  it('keeps override disabled until both a reason and a direction are given', async () => {
    const mock = stubFetch()
    render(<VerdictForm review={makeReview()} user={admin} onDecided={vi.fn()} />)
    fireEvent.click(screen.getByTestId('verdict-override'))
    const submit = screen.getByTestId('verdict-submit')
    expect(submit).toBeDisabled()

    fireEvent.change(screen.getByTestId('verdict-reason'),
      { target: { value: 'the table is tiny' } })
    expect(submit).toBeDisabled()

    fireEvent.change(screen.getByTestId('verdict-reason'), { target: { value: '   ' } })
    fireEvent.change(screen.getByTestId('verdict-direction'),
      { target: { value: 'too_strict' } })
    expect(submit).toBeDisabled() // a blank reason is no reason

    fireEvent.change(screen.getByTestId('verdict-reason'),
      { target: { value: 'the table is tiny' } })
    expect(submit).toBeEnabled()
    fireEvent.click(submit)
    await waitFor(() => expect(mock).toHaveBeenCalled())
    expect(posted(mock)).toEqual({ verdict: 'override', direction: 'too_strict',
      reason: 'the table is tiny' })
  })

  it('offers exactly the two override directions', () => {
    stubFetch()
    render(<VerdictForm review={makeReview()} user={admin} />)
    fireEvent.click(screen.getByTestId('verdict-override'))
    const values = Array.from(screen.getByTestId('verdict-direction')
      .querySelectorAll('option')).map(o => o.value)
    expect(values).toEqual(['', 'too_strict', 'too_lenient'])
  })

  it('sends concur with edits with its optional reason and no direction', async () => {
    const mock = stubFetch()
    render(<VerdictForm review={makeReview()} user={operator} onDecided={vi.fn()} />)
    fireEvent.click(screen.getByTestId('verdict-concur-with-edits'))
    expect(screen.queryByTestId('verdict-direction')).toBeNull()
    fireEvent.change(screen.getByTestId('verdict-reason'),
      { target: { value: 'add lock_timeout' } })
    fireEvent.click(screen.getByTestId('verdict-submit'))
    await waitFor(() => expect(mock).toHaveBeenCalled())
    expect(posted(mock)).toEqual({ verdict: 'concur_with_edits',
      reason: 'add lock_timeout' })
  })

  it('shows who decided first on a 409', async () => {
    stubFetch(() => response(409, { error: 'change review already decided',
      code: 'already_decided', decided_by: 'user:7', dba_verdict: 'override' }))
    const onDecided = vi.fn()
    render(<VerdictForm review={makeReview()} user={operator} onDecided={onDecided} />)
    fireEvent.click(screen.getByTestId('verdict-concur'))
    fireEvent.click(screen.getByTestId('verdict-submit'))
    expect(await screen.findByTestId('verdict-error'))
      .toHaveTextContent('already decided by user:7')
    expect(onDecided).not.toHaveBeenCalled()
  })

  it('shows the server message on other errors and lets the DBA retry', async () => {
    let calls = 0
    stubFetch(() => {
      calls += 1
      return calls === 1
        ? response(400, { error: 'override needs a reason', code: 'invalid_request' })
        : response(200, makeReview({ dba_verdict: 'concur' }))
    })
    const onDecided = vi.fn()
    render(<VerdictForm review={makeReview()} user={operator} onDecided={onDecided} />)
    fireEvent.click(screen.getByTestId('verdict-concur'))
    fireEvent.click(screen.getByTestId('verdict-submit'))
    expect(await screen.findByTestId('verdict-error'))
      .toHaveTextContent('override needs a reason')
    fireEvent.click(screen.getByTestId('verdict-submit'))
    await waitFor(() => expect(onDecided).toHaveBeenCalled())
  })

  it('shows a network failure', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch') }))
    render(<VerdictForm review={makeReview()} user={operator} />)
    fireEvent.click(screen.getByTestId('verdict-concur'))
    fireEvent.click(screen.getByTestId('verdict-submit'))
    expect(await screen.findByTestId('verdict-error')).toHaveTextContent('Failed to fetch')
  })

  it('shows a recorded verdict instead of the buttons', () => {
    stubFetch()
    render(<VerdictForm user={admin} review={makeReview({ dba_verdict: 'override',
      dba_direction: 'too_lenient', dba_reason: 'missed the trigger',
      dba_actor: 'user:4', dba_decided_at: '2026-10-23T10:00:00Z' })} />)
    expect(screen.queryByTestId('verdict-concur')).toBeNull()
    const decided = screen.getByTestId('verdict-decided')
    expect(decided).toHaveTextContent('Override (too lenient)')
    expect(decided).toHaveTextContent('user:4')
    expect(decided).toHaveTextContent('missed the trigger')
  })

  it('names the revision a carried-over verdict came from', () => {
    stubFetch()
    render(<VerdictForm user={admin} review={makeReview({ dba_verdict: 'concur',
      dba_actor: 'user:4', verdict_carried_from: '99999999-9999-4999-8999-999999999999' })} />)
    expect(screen.getByTestId('verdict-decided')).toHaveTextContent(/carried over/i)
    expect(screen.getByTestId('verdict-decided')).toHaveTextContent('99999999')
  })

  it('offers no verdict on a review that is not complete', () => {
    stubFetch()
    const { rerender } = render(<VerdictForm user={admin}
      review={makeReview({ state: 'running' })} />)
    expect(screen.queryByTestId('verdict-concur')).toBeNull()
    rerender(<VerdictForm user={admin} review={makeReview({ state: 'failed' })} />)
    expect(screen.queryByTestId('verdict-concur')).toBeNull()
  })

  it('cancels a started verdict', () => {
    const mock = stubFetch()
    render(<VerdictForm review={makeReview()} user={admin} />)
    fireEvent.click(screen.getByTestId('verdict-override'))
    fireEvent.click(screen.getByTestId('verdict-cancel'))
    expect(screen.queryByTestId('verdict-submit')).toBeNull()
    expect(mock).not.toHaveBeenCalled()
  })
})
