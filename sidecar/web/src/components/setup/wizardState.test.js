import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  AUTONOMY_CHOICES, SKIP_COST, WIZARD_STEPS, channelTestMessage, llmTestMessage,
  pickWizardEntry, readSkipped, stepStatus, wizardFinished, writeSkipped,
} from './wizardState'

// ux-04b: the wizard's step status comes from the same checklist the
// onboarding route serves, so the two never disagree.

const steps = (done = {}) => [
  { id: 'connected', done: Boolean(done.connected) },
  { id: 'extensions', done: true },
  { id: 'first_look', done: true },
  { id: 'model', done: Boolean(done.model), optional: true },
  { id: 'mcp_token', done: false, optional: true },
  { id: 'notifications', done: Boolean(done.notifications), optional: true },
  { id: 'grant_more', done: Boolean(done.grant_more) },
]
const entry = (done, extra = {}) => ({ database: 'app', install_kind: 'new',
  steps: steps(done), setup: {}, ...extra })

describe('stepStatus', () => {
  it('walks not started, connected, model, channel, autonomy, done', () => {
    const seen = []
    const states = [{}, { connected: true }, { connected: true, model: true },
      { connected: true, model: true, notifications: true },
      { connected: true, model: true, notifications: true, grant_more: true }]
    for (const done of states) {
      const s = stepStatus(entry(done), [])
      seen.push(WIZARD_STEPS.filter(k => s[k] === 'done').length)
    }
    expect(seen).toEqual([0, 1, 2, 3, 4])
    expect(wizardFinished(stepStatus(entry(states[4]), []))).toBe(true)
    expect(wizardFinished(stepStatus(entry(states[3]), []))).toBe(false)
  })

  it('counts the recorded autonomy choice as done while trust stays observation', () => {
    const s = stepStatus(entry({}, { setup: { autonomy_chosen_at: '2026-11-16T10:00:00Z' } }),
      [])
    expect(s.autonomy).toBe('done')
  })

  it('marks a skip, and a later done wins over it', () => {
    expect(stepStatus(entry({}), ['model']).model).toBe('skipped')
    expect(stepStatus(entry({ model: true }), ['model']).model).toBe('done')
  })

  it('moves the channel step back to not done when the channel is gone', () => {
    expect(stepStatus(entry({ notifications: true }), []).channel).toBe('done')
    expect(stepStatus(entry({ notifications: false }), []).channel).toBe('todo')
  })

  it('treats a missing entry (no database yet) as nothing done', () => {
    const s = stepStatus(null, [])
    expect(WIZARD_STEPS.map(k => s[k])).toEqual(['todo', 'todo', 'todo', 'todo'])
    expect(stepStatus({ database: 'app' }, null).connect).toBe('todo')
  })
})

describe('pickWizardEntry', () => {
  const skipsNone = () => []
  it('picks the first new install whose wizard is unfinished', () => {
    const done = { connected: true, model: true, notifications: true, grant_more: true }
    const entries = [entry(done, { database: 'a' }), entry({}, { database: 'b' }),
      entry({}, { database: 'c' })]
    expect(pickWizardEntry(entries, skipsNone)?.database).toBe('b')
  })

  it('never picks an existing install, an unread database or a finished one', () => {
    expect(pickWizardEntry([entry({}, { install_kind: 'existing' })], skipsNone)).toBeNull()
    expect(pickWizardEntry([entry({}, { error: 'unreachable' })], skipsNone)).toBeNull()
    const skippedAll = () => ['connect', 'model', 'channel', 'autonomy']
    expect(pickWizardEntry([entry({})], skippedAll)).toBeNull()
    expect(pickWizardEntry([], skipsNone)).toBeNull()
    expect(pickWizardEntry(undefined, skipsNone)).toBeNull()
  })
})

describe('skip storage', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    window.localStorage.clear()
  })

  it('round-trips per database and ignores unknown step names', () => {
    writeSkipped('app', ['model', 'bogus', 'channel'])
    expect(readSkipped('app')).toEqual(['model', 'channel'])
    expect(readSkipped('other')).toEqual([])
  })

  it('survives blocked or corrupt storage', () => {
    window.localStorage.setItem('pg_sage.setup.skipped.app', '{not json')
    expect(readSkipped('app')).toEqual([])
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError')
    })
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError')
    })
    expect(readSkipped('app')).toEqual([])
    expect(() => writeSkipped('app', ['model'])).not.toThrow()
  })
})

describe('messages', () => {
  it('says what skipping each step costs', () => {
    expect(SKIP_COST.model).toBe('Skipped: LLM features stay off until a model is set')
    for (const k of WIZARD_STEPS) expect(SKIP_COST[k]).toMatch(/^Skipped: /)
  })

  it.each([
    [{ class: 'ok', latency_ms: 420, model: 'gpt-x' }, 200, /answered in 420 ms/],
    [{ class: 'auth', error: 'HTTP 401' }, 200, /refused the key/],
    [{ class: 'rate' }, 200, /rate limiting/],
    [{ class: 'timeout' }, 200, /did not answer in time/],
    [{ class: 'circuit_open' }, 200, /circuit/],
    [{ class: 'budget' }, 200, /budget/],
    [{ class: 'something_new' }, 200, /something_new/],
    [null, 503, /not configured/],
    [null, 403, /admin/],
    [null, 500, /500/],
  ])('llm/test %j (%i) reads %s', (body, status, want) => {
    expect(llmTestMessage(body, status).text).toMatch(want)
  })

  it('only an ok class passes', () => {
    expect(llmTestMessage({ class: 'ok' }, 200).ok).toBe(true)
    expect(llmTestMessage({ class: 'auth' }, 200).ok).toBe(false)
    expect(llmTestMessage(null, 503).ok).toBe(false)
  })

  it('a failed channel test shows the HTTP status', () => {
    expect(channelTestMessage(502, 'webhook returned 500')).toBe(
      'Test failed (HTTP 502): webhook returned 500')
    expect(channelTestMessage(404, '')).toBe('Test failed (HTTP 404)')
  })
})

describe('AUTONOMY_CHOICES', () => {
  it('maps the three plain choices onto trust.level and execution_mode (decision D6)', () => {
    expect(AUTONOMY_CHOICES.map(c => [c.key, c.label, c.trust, c.mode])).toEqual([
      ['watch', 'Watch only', 'observation', 'manual'],
      ['ask', 'Ask me first', 'advisory', 'approval'],
      ['safe', 'Do safe things and tell me', 'advisory', 'auto'],
    ])
    for (const c of AUTONOMY_CHOICES) {
      expect(c.sentence('orders')).toContain('orders')
    }
    expect(AUTONOMY_CHOICES.filter(c => c.confirm).map(c => c.key)).toEqual(['safe'])
  })
})
