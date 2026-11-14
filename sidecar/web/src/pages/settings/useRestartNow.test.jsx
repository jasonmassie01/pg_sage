import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useRestartNow } from './useRestartNow'
import { ToastProvider } from '../../components/Toast'
import { answerConfirm } from '../../test/confirmDialog'

// dashboard-30: the restart hook moved out of SettingsPage. A failed or
// unsupported restart must say why and never reload the page.

function Harness() {
  const { restarting, restartNow, dialog } = useRestartNow()
  return (
    <div>
      <button data-testid="go" onClick={restartNow}>go</button>
      <span data-testid="state">{restarting ? 'restarting' : 'idle'}</span>
      {dialog}
    </div>
  )
}

const realSetTimeout = globalThis.setTimeout

// spyTimers records setTimeout calls and drops the 6 s reload, which jsdom
// cannot perform.
const spyTimers = () => vi.spyOn(globalThis, 'setTimeout')
  .mockImplementation((fn, ms, ...args) => (ms === 6000 ? 0 : realSetTimeout(fn, ms, ...args)))

const restartCalls = () => globalThis.fetch.mock.calls.filter(([u]) => u === '/api/v1/restart')

function start() {
  render(<ToastProvider><Harness /></ToastProvider>)
  screen.getByTestId('go').click()
}

beforeEach(() => {
  vi.restoreAllMocks()
  globalThis.fetch = vi.fn()
})

describe('useRestartNow', () => {
  it('sends nothing when the operator cancels', async () => {
    start()
    expect(await answerConfirm('cancel')).toBe('Restart the pg_sage sidecar now?')
    expect(restartCalls()).toHaveLength(0)
    expect(screen.getByTestId('state')).toHaveTextContent('idle')
  })

  it.each([
    ['an unsupervised sidecar', { ok: false, status: 501, json: async () => ({}) },
      'Restart not supported: no supervisor configured'],
    ['a refusal with a message',
      { ok: false, status: 403, json: async () => ({ error: 'admins only' }) }, 'admins only'],
    ['a refusal without a body', {
      ok: false, status: 500, json: async () => { throw new SyntaxError('x') },
    }, 'Restart failed'],
  ])('reports %s and does not reload', async (_, answer, want) => {
    globalThis.fetch.mockResolvedValueOnce(answer)
    const timer = spyTimers()
    start()
    await answerConfirm()
    expect(restartCalls()).toHaveLength(1)
    expect(await screen.findByText(want)).toBeInTheDocument()
    expect(timer.mock.calls.some(([, ms]) => ms === 6000)).toBe(false)
    expect(screen.getByTestId('state')).toHaveTextContent('idle')
  })

  it('reports a network failure', async () => {
    globalThis.fetch.mockRejectedValueOnce(new TypeError('Failed to fetch'))
    start()
    await answerConfirm()
    expect(await screen.findByText('Failed to fetch')).toBeInTheDocument()
    expect(screen.getByTestId('state')).toHaveTextContent('idle')
  })

  it('announces the restart and schedules the reload', async () => {
    globalThis.fetch.mockResolvedValueOnce({ ok: true, status: 202, json: async () => ({}) })
    const timer = spyTimers()
    start()
    await answerConfirm()
    expect(await screen.findByText('Restarting — reconnecting in a few seconds…'))
      .toBeInTheDocument()
    expect(timer.mock.calls.some(([, ms]) => ms === 6000)).toBe(true)
    const [, init] = restartCalls()[0]
    expect(init).toEqual({
      method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' },
    })
  })
})
