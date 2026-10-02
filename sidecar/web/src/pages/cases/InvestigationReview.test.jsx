import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { InvestigationReview } from './InvestigationReview'

// Phase 1.1: an operator accepts or rejects a finished investigation's
// diagnosis from the Cases panel, optionally with a note and the actual
// root cause. One request records the shadow review (earned autonomy)
// and the investigation outcome (incident memory).

const concluded = {
  id: '11111111-1111-4111-8111-111111111111', state: 'concluded',
  trigger_kind: 'lock_blocking', summary: { family: 'lock_blocking', root: 'idle_in_tx_holder' },
}

function okResponse(body) {
  return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve(body) })
}

afterEach(() => vi.unstubAllGlobals())

function renderReview(investigation = concluded, role = 'operator') {
  return render(<InvestigationReview database="orders" investigation={investigation}
    user={{ role }} />)
}

describe('InvestigationReview', () => {
  it('is offered to operators and admins on finished investigations only', () => {
    const { unmount } = renderReview(concluded, 'viewer')
    expect(screen.queryByTestId('investigation-review')).toBeNull()
    unmount()
    for (const state of ['collecting', 'failed', 'paused']) {
      const r = renderReview({ ...concluded, state })
      expect(screen.queryByTestId('investigation-review')).toBeNull()
      r.unmount()
    }
    for (const [state, role] of [['concluded', 'operator'], ['inconclusive', 'admin']]) {
      const r = renderReview({ ...concluded, state }, role)
      expect(screen.getByRole('button', { name: /accept diagnosis/i })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: /reject diagnosis/i })).toBeInTheDocument()
      r.unmount()
    }
  })

  it('accepts a diagnosis for the investigation database', async () => {
    const fetch = vi.fn(() => okResponse({ ok: true, family: 'lock_blocking',
      verdict: 'accepted', investigation_outcome: { verdict: 'confirmed' } }))
    vi.stubGlobal('fetch', fetch)
    renderReview()
    fireEvent.click(screen.getByRole('button', { name: /accept diagnosis/i }))
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1))
    const [url, opts] = fetch.mock.calls[0]
    expect(url).toBe('/api/v1/sre/autonomy/reviews')
    expect(opts.method).toBe('POST')
    expect(opts.credentials).toBe('include')
    expect(opts.headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(opts.body)).toEqual({
      database: 'orders', investigation_id: concluded.id, verdict: 'accepted',
      note: '', actual_root_cause: '',
    })
    const status = await screen.findByTestId('investigation-review-status')
    expect(status).toHaveTextContent(/accepted/i)
    expect(status).toHaveTextContent('lock_blocking')
    expect(status).toHaveTextContent(/confirmed/i)
  })

  it('rejects with a note and the actual root cause', async () => {
    const fetch = vi.fn(() => okResponse({ ok: true, family: 'lock_blocking',
      verdict: 'rejected', actual_node: 'connection_leak',
      investigation_outcome: { verdict: 'refuted', actual_node: 'connection_leak' } }))
    vi.stubGlobal('fetch', fetch)
    renderReview()
    fireEvent.change(screen.getByLabelText(/note/i), { target: { value: ' wrong holder ' } })
    fireEvent.change(screen.getByLabelText(/actual root cause/i),
      { target: { value: 'connection_leak' } })
    fireEvent.click(screen.getByRole('button', { name: /reject diagnosis/i }))
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1))
    expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({
      database: 'orders', investigation_id: concluded.id, verdict: 'rejected',
      note: 'wrong holder', actual_root_cause: 'connection_leak',
    })
    const status = await screen.findByTestId('investigation-review-status')
    expect(status).toHaveTextContent(/rejected/i)
    expect(status).toHaveTextContent('connection_leak')
  })

  it('says when the investigation outcome was not recorded', async () => {
    vi.stubGlobal('fetch', vi.fn(() => okResponse({ ok: true, family: 'lock_blocking',
      verdict: 'accepted',
      investigation_outcome_skipped: 'an inconclusive investigation is confirmed only ' +
        'with a graph node as the actual root cause' })))
    renderReview({ ...concluded, state: 'inconclusive' })
    fireEvent.click(screen.getByRole('button', { name: /accept diagnosis/i }))
    expect(await screen.findByTestId('investigation-review-status'))
      .toHaveTextContent(/graph node/)
  })

  it('shows the server error and keeps the form', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve({ ok: false, status: 409,
      json: () => Promise.resolve({ error: 'only a finished investigation can be reviewed',
        code: 'not_concluded' }) })))
    renderReview()
    fireEvent.click(screen.getByRole('button', { name: /reject diagnosis/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/finished investigation/)
    expect(screen.getByRole('button', { name: /reject diagnosis/i })).not.toBeDisabled()
  })

  it('falls back to the status when the error body is not JSON', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve({ ok: false, status: 503,
      json: () => Promise.reject(new Error('not json')) })))
    renderReview()
    fireEvent.click(screen.getByRole('button', { name: /accept diagnosis/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent('503')
  })

  it('sends one request per click while it is in flight', async () => {
    let resolve
    const fetch = vi.fn(() => new Promise(r => { resolve = r }))
    vi.stubGlobal('fetch', fetch)
    renderReview()
    const accept = screen.getByRole('button', { name: /accept diagnosis/i })
    fireEvent.click(accept)
    fireEvent.click(accept)
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(accept).toBeDisabled()
    resolve({ ok: true, status: 200, json: () => Promise.resolve({ verdict: 'accepted' }) })
    await waitFor(() => expect(accept).not.toBeDisabled())
  })
})
