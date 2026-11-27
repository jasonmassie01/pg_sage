import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AskPage } from '../AskPage'
import {
  BACKOFF_AFTER_MS, POLL_FAST_MS, POLL_SLOW_MS, pollDelay, readRunParam, runOutcomeError,
  writeRunParam,
} from './useAskRun'
import { BASE, makeAnswer, operator, posts, response, stubAsk } from './testkit'

// mcp-07b: the page asks through asynchronous runs. It starts a run,
// polls it (1 s, then 5 s after 30 s), shows the progress steps and a
// Cancel button that posts once, and resumes polling after a reload from
// ?run=. fetch is stubbed per route; no server is involved.

const RUN = 'run-7'

function startsRun(polls) {
  let n = 0
  return {
    runs: () => response(202, { run_id: RUN, conversation_id: 'c-1' }),
    run: () => polls[Math.min(n++, polls.length - 1)](),
  }
}

const running = progress => () => response(200, { id: RUN, state: 'running', progress })
const step = (tool, status, actionID) => ({ tool, status, action_id: actionID,
  at: '2026-11-13T10:00:00Z' })

async function ask(text) {
  fireEvent.change(screen.getByTestId('ask-input'), { target: { value: text } })
  fireEvent.click(screen.getByTestId('ask-submit'))
}

beforeEach(() => window.history.replaceState(null, '', '/#/ask?database=orders'))
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('Ask Sage runs', () => {
  it('renders the progress steps of a polled run, then its answer', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const mock = stubAsk(startsRun([
      running([]),
      running([step('list_findings', 'ok')]),
      running([step('list_findings', 'ok'), step('propose_action', 'queued', '9')]),
      () => response(200, { id: RUN, state: 'done', progress: [], answer: makeAnswer() }),
    ]))
    render(<AskPage database="orders" user={operator} />)
    await ask('Why is orders slow?')
    expect(await screen.findByTestId('ask-run')).toBeInTheDocument()
    expect(readRunParam()).toBe(RUN)
    await act(() => vi.advanceTimersByTimeAsync(POLL_FAST_MS))
    expect(await screen.findByTestId('ask-run-step-0')).toHaveTextContent('list_findings')
    await act(() => vi.advanceTimersByTimeAsync(POLL_FAST_MS))
    expect(await screen.findByTestId('ask-run-step-1')).toHaveTextContent('action 9')
    await act(() => vi.advanceTimersByTimeAsync(POLL_FAST_MS))
    expect(await screen.findByTestId('ask-answer-41')).toBeInTheDocument()
    expect(screen.queryByTestId('ask-run')).toBeNull()
    expect(readRunParam()).toBe('')
    expect(posts(mock).map(([url]) => String(url))).toEqual([`${BASE}/runs`])
    await waitFor(() => expect(screen.getByTestId('ask-submit')).toBeDisabled())
  })

  it('posts Cancel once and shows the partial answer of a run that acted', async () => {
    let cancelled = false
    const partial = makeAnswer({ status: 'incomplete', stop: 'cancelled' })
    const mock = stubAsk({
      ...startsRun([running([step('propose_action', 'queued', '9')])]),
      cancel: () => {
        cancelled = true
        return response(200, { id: RUN, state: 'cancelled', progress: [], answer: partial })
      },
    })
    render(<AskPage database="orders" user={operator} />)
    await ask('Fix it')
    const button = await screen.findByTestId('ask-run-cancel')
    fireEvent.click(button)
    fireEvent.click(button)
    await screen.findByTestId('ask-answer-41')
    const cancels = posts(mock).filter(([url]) => String(url).endsWith('/cancel'))
    expect(cancels).toHaveLength(1)
    expect(String(cancels[0][0])).toBe(`${BASE}/runs/${RUN}/cancel`)
    expect(cancelled).toBe(true)
    expect(screen.queryByTestId('ask-run')).toBeNull()
  })

  it('says so when a run is cancelled before it acted', async () => {
    stubAsk({
      ...startsRun([running([])]),
      cancel: () => response(200, { id: RUN, state: 'cancelled', progress: [] }),
    })
    render(<AskPage database="orders" user={operator} />)
    await ask('Anything?')
    fireEvent.click(await screen.findByTestId('ask-run-cancel'))
    expect(await screen.findByTestId('ask-error')).toHaveTextContent(/cancelled before/i)
  })

  it('shows a failed run as an error, not an answer', async () => {
    stubAsk(startsRun([() => response(200, { id: RUN, state: 'failed', progress: [],
      error: 'ask: unavailable: record the run progress' })]))
    render(<AskPage database="orders" user={operator} />)
    await ask('Anything?')
    expect(await screen.findByTestId('ask-error')).toHaveTextContent('record the run progress')
    expect(screen.queryByTestId('ask-answer-41')).toBeNull()
  })

  it('resumes polling the run named in ?run= after a reload', async () => {
    window.history.replaceState(null, '', `/#/ask?database=orders&run=${RUN}`)
    const mock = stubAsk({ run: () => response(200, { id: RUN, state: 'done',
      progress: [], answer: makeAnswer({ id: 77 }) }) })
    render(<AskPage database="orders" user={operator} />)
    expect(await screen.findByTestId('ask-answer-77')).toBeInTheDocument()
    expect(posts(mock)).toHaveLength(0)
    expect(readRunParam()).toBe('')
  })

  it('shows the busy and conflict refusals of a start', async () => {
    stubAsk({ runs: () => response(429, { error: 'ask: too many runs in progress',
      code: 'ask_busy' }) })
    render(<AskPage database="orders" user={operator} />)
    await ask('Anything?')
    expect(await screen.findByTestId('ask-error')).toHaveTextContent('too many runs')
    expect(screen.queryByTestId('ask-run')).toBeNull()
  })
})

describe('run polling helpers', () => {
  it('polls every second, then every five after thirty seconds', () => {
    expect(pollDelay(0)).toBe(POLL_FAST_MS)
    expect(pollDelay(BACKOFF_AFTER_MS - 1)).toBe(POLL_FAST_MS)
    expect(pollDelay(BACKOFF_AFTER_MS)).toBe(POLL_SLOW_MS)
    expect([POLL_FAST_MS, POLL_SLOW_MS, BACKOFF_AFTER_MS]).toEqual([1000, 5000, 30000])
  })

  it('keeps the other hash parameters when it writes and clears ?run=', () => {
    window.history.replaceState(null, '', '/#/ask?database=orders')
    writeRunParam('r 1')
    expect(window.location.hash).toBe('#/ask?database=orders&run=r+1')
    expect(readRunParam()).toBe('r 1')
    writeRunParam('')
    expect(window.location.hash).toBe('#/ask?database=orders')
    expect(readRunParam()).toBe('')
  })

  it('describes the runs that ended without concluding', () => {
    expect(runOutcomeError({ state: 'done', answer: {} })).toBeNull()
    expect(runOutcomeError({ state: 'cancelled', answer: {} })).toBeNull()
    expect(runOutcomeError({ state: 'interrupted' }).message).toMatch(/restarted/)
    expect(runOutcomeError({ state: 'failed' }).message).toMatch(/unknown error/)
  })
})
