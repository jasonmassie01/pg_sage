import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { InvestigationPanel } from './InvestigationPanel'

// sre-09: an open investigation detail loaded once, so a run still
// collecting or evaluating stayed frozen until the operator closed and
// reopened it. A live run is fetched again every 5 s; a terminal one is
// not fetched again. The real useAPI runs against a fetch double under
// fake timers (InvestigationPanel.test.jsx mocks useAPI for the rest).

vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const DETAIL = '/api/v1/databases/orders/investigations/inv-live'

const summary = (state) => ({ id: 'inv-live', state, trigger_kind: 'lock_blocking',
  summary: { family: 'lock_blocking' } })
const collecting = { investigation: summary('collecting'), hypotheses: [], evidence: [] }
const concluded = {
  investigation: { ...summary('concluded'),
    summary: { family: 'lock_blocking', conclusive: true } },
  hypotheses: [{ node: 'idle_in_tx_holder', label: 'idle-in-transaction holder',
    status: 'root_cause', confidence: 0.85, support: [], contradict: [] }],
  evidence: [],
}

let detailReplies = []
let detailCalls = 0

function reply(body, status = 200) {
  return Promise.resolve(new Response(JSON.stringify(body), { status,
    headers: { 'Content-Type': 'application/json' } }))
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-11-11T10:00:00Z'))
  detailCalls = 0
  vi.stubGlobal('fetch', vi.fn(input => {
    const url = typeof input === 'string' ? input : input.url
    if (url !== DETAIL) return reply({ items: [], events: [] })
    const next = detailReplies[Math.min(detailCalls, detailReplies.length - 1)]
    detailCalls += 1
    return next.status ? reply({ error: 'boom' }, next.status) : reply(next)
  }))
})
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const advance = ms => act(async () => { await vi.advanceTimersByTimeAsync(ms) })

async function openPanel(state) {
  render(<InvestigationPanel database="orders" investigation={summary(state)}
    user={{ role: 'viewer' }} />)
  fireEvent.click(screen.getByTestId('investigation-toggle'))
  await advance(0)
}

describe('a live investigation detail', () => {
  it('is fetched again at 5 s, turns concluded, then is not fetched again', async () => {
    detailReplies = [collecting, concluded]
    await openPanel('collecting')
    expect(detailCalls).toBe(1)
    expect(screen.queryByText(/idle-in-transaction holder/)).toBeNull()
    await advance(4900)
    expect(detailCalls).toBe(1)
    await advance(100)
    expect(detailCalls).toBeGreaterThanOrEqual(2)
    expect(screen.getAllByText(/idle-in-transaction holder/).length).toBeGreaterThan(0)
    const settled = detailCalls
    expect(settled).toBeLessThanOrEqual(3)
    await advance(60000)
    expect(detailCalls).toBe(settled)
  })

  it('never polls a run that was already terminal', async () => {
    detailReplies = [concluded]
    await openPanel('concluded')
    await advance(60000)
    expect(detailCalls).toBe(1)
    expect(screen.getAllByText(/idle-in-transaction holder/).length).toBeGreaterThan(0)
  })

  it('polls a run the list still shows as live once its detail says so', async () => {
    // The fleet list (30 s) may lag: the detail's own state decides.
    detailReplies = [collecting, collecting, concluded]
    await openPanel('queued')
    await advance(5000)
    await advance(5000)
    expect(screen.getAllByText(/idle-in-transaction holder/).length).toBeGreaterThan(0)
    const settled = detailCalls
    await advance(30000)
    expect(detailCalls).toBe(settled)
  })

  it('keeps the last detail on screen when a refresh fails', async () => {
    detailReplies = [collecting, { status: 500 }, concluded]
    await openPanel('evaluating')
    await advance(5000)
    expect(screen.getByTestId('investigation-detail')).toBeInTheDocument()
    expect(screen.getByTestId('investigation-refresh-failed'))
      .toHaveTextContent('boom')
    await advance(5000)
    expect(screen.getAllByText(/idle-in-transaction holder/).length).toBeGreaterThan(0)
    expect(screen.queryByTestId('investigation-refresh-failed')).toBeNull()
  })
})
