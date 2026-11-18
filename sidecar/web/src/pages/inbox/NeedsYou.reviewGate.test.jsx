import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { NeedsYou } from './NeedsYou'

// review-05b: the scorecard sends an admin to the Inbox to approve a G-M1
// promotion (review-05a's item "review-gate-<id>" from sage.review_gate).
// Approving it there must reach POST .../review/gate/{id}/approve, not the
// SRE autonomy ledger, which does not know the id. No concurrent access
// tests: one dialog sends one request; two admins approving at once is
// modelgate's TestApprove (only the newest pending row can be granted).

vi.mock('../../hooks/useAPI', () => ({ useAPI: () => ({ data: null, error: null }) }))

const item = (id, extra = {}) => ({ kind: 'autonomy_promotion', id, database: 'orders db',
  title: `promotion ${id}`, why: 'G-M1: 50 of 50 reviews', risk: 'high',
  created_at: '2026-11-16T10:00:00Z', href: '#/trust', source: 'sage.review_gate',
  actionable: true, ...extra })

let posts
function stubFetch(status = 200, body = { verdict: 'granted' }) {
  posts = []
  vi.stubGlobal('fetch', vi.fn(async (url, init = {}) => {
    posts.push({ url, method: init.method, headers: init.headers, body: init.body })
    return { ok: status >= 200 && status < 300, status, statusText: '',
      json: async () => body, text: async () => JSON.stringify(body) }
  }))
}

async function approve(phrase = 'orders db') {
  fireEvent.click(screen.getByRole('button', { name: 'Approve' }))
  fireEvent.change(screen.getByTestId('confirm-dialog-phrase'), { target: { value: phrase } })
  await act(async () => { fireEvent.click(screen.getByTestId('confirm-dialog-confirm')) })
}

beforeEach(() => stubFetch())
afterEach(() => vi.unstubAllGlobals())

describe('NeedsYou model gate promotion', () => {
  it('approves a review gate item through the review gate route', async () => {
    const onDecided = vi.fn()
    render(<NeedsYou items={[item('review-gate-7')]} onDecided={onDecided} />)
    await approve()
    expect(posts).toHaveLength(1)
    expect(posts[0].url).toBe('/api/v1/databases/orders%20db/review/gate/7/approve')
    expect(posts[0].method).toBe('POST')
    expect(posts[0].headers['Content-Type']).toBe('application/json')
    expect(posts[0].headers['X-Sage-Surface']).toBe('inbox')
    await waitFor(() => expect(onDecided).toHaveBeenCalledTimes(1))
  })

  it('keeps a ledger promotion on the SRE autonomy route', async () => {
    render(<NeedsYou items={[item('p-3', { source: 'autonomy' })]} onDecided={vi.fn()} />)
    await approve()
    expect(posts[0].url).toBe('/api/v1/sre/autonomy/proposals/p-3/approve?database=orders%20db')
  })

  it('a malformed review gate id is refused before any request', async () => {
    for (const id of ['review-gate-', 'review-gate-0', 'review-gate-7x', 'review-gate--1']) {
      const { unmount } = render(<NeedsYou items={[item(id)]} onDecided={vi.fn()} />)
      await approve()
      expect(posts).toEqual([])
      expect(screen.getByRole('dialog')).toHaveTextContent('not a valid model gate id')
      unmount()
    }
  })

  it('a 409 from the gate route is shown and the item is not decided', async () => {
    stubFetch(409, { error: 'gate evaluation 7 is not the newest pending G-M1 evaluation' })
    const onDecided = vi.fn()
    render(<NeedsYou items={[item('review-gate-7')]} onDecided={onDecided} />)
    await approve()
    expect(screen.getByRole('dialog'))
      .toHaveTextContent('gate evaluation 7 is not the newest pending G-M1 evaluation')
    expect(onDecided).not.toHaveBeenCalled()
  })

  it('a HIGH promotion is not sent until the phrase is typed exactly', async () => {
    render(<NeedsYou items={[item('review-gate-7')]} onDecided={vi.fn()} />)
    await approve('orders')
    expect(posts).toEqual([])
    expect(screen.getByTestId('confirm-dialog-confirm')).toBeDisabled()
  })
})
