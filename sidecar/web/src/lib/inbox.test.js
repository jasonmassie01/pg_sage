import { describe, expect, it } from 'vitest'
import {
  changesSince, goalSentence, inboxBadge, kindLabel, sinceLabel, sinceWindow, sourceErrorText,
  withSurface,
} from './inbox'

// ux-01b: the Inbox's wording and window arithmetic. No concurrent access
// tests: pure functions.

const local = (y, m, d, h = 12) => new Date(y, m - 1, d, h, 0, 0).getTime()

describe('sinceWindow', () => {
  it('starts a first visit, or an unreadable time, 24 hours back', () => {
    for (const seen of [null, undefined, '', 'not a time']) {
      expect(sinceWindow(seen, 100 * 3600e3)).toEqual(
        { since: '24h', from: 76 * 3600e3, firstVisit: true })
    }
  })

  it('reads back to the seen time plus an hour, capped at the ledger maximum', () => {
    const now = Date.parse('2026-11-12T18:00:00Z')
    expect(sinceWindow('2026-11-12T18:00:00Z', now))
      .toEqual({ since: '3600s', from: now, firstVisit: false })
    expect(sinceWindow('2025-01-01T00:00:00Z', now).since).toBe(`${9600 * 3600}s`)
    // A seen time ahead of the page clock still reads the slack window.
    expect(sinceWindow('2026-11-12T19:00:00Z', now).since).toBe('3600s')
  })
})

describe('changesSince', () => {
  it('keeps rows at or after the time once each, and drops bad times', () => {
    const from = Date.parse('2026-11-10T15:00:00Z')
    const rows = [
      { database: 'a', action_id: 1, executed_at: '2026-11-10T15:00:00Z' },
      { database: 'a', action_id: 1, executed_at: '2026-11-10T15:00:00Z' },
      { database: 'b', action_id: 1, executed_at: '2026-11-11T00:00:00Z' },
      { database: 'a', action_id: 2, executed_at: '2026-11-10T14:59:59.999Z' },
      { database: 'a', action_id: 3, executed_at: 'garbage' },
    ]
    expect(changesSince(rows, from).map(r => `${r.database}${r.action_id}`))
      .toEqual(['a1', 'b1'])
    expect(changesSince(null, from)).toEqual([])
  })
})

describe('labels and sentences', () => {
  it('words the seen time by how long ago it was', () => {
    const now = local(2026, 11, 12, 18)
    expect(sinceLabel(local(2026, 11, 12, 9), now)).toBe('since 09:00')
    expect(sinceLabel(local(2026, 11, 10), now)).toBe('since Tuesday')
    expect(sinceLabel(local(2026, 11, 3), now)).toBe('since Nov 3')
  })

  it('builds the goal state from counts', () => {
    expect(goalSentence([], 'since Tuesday')).toBe('Nothing needs you.')
    expect(goalSentence([{ verdict: 'regressed' }], 'since Tuesday'))
      .toBe('Nothing needs you. 1 change since Tuesday, all verified.')
    expect(goalSentence([{ verdict: 'pending' }, { verdict: 'unverifiable' }], 'today'))
      .toBe('Nothing needs you. 2 changes today, 0 verified.')
  })

  it('names failed sources once each', () => {
    expect(sourceErrorText([])).toBe('')
    expect(sourceErrorText(undefined)).toBe('')
    expect(sourceErrorText([{ source: 'fact', error: 'x' }, { source: 'fact', error: 'y' },
      { source: 'policy_exception', database: 'b', error: 'z' }, { source: 'weird' }]))
      .toBe('Could not read: facts, policy exceptions (b), weird')
  })

  it('labels kinds and adds the surface header', () => {
    expect(kindLabel('runbook_signature')).toBe('Runbook signature')
    expect(kindLabel('unknown_kind')).toBe('unknown_kind')
    expect(withSurface({ a: '1' }, 'inbox')).toEqual({ a: '1', 'X-Sage-Surface': 'inbox' })
    expect(withSurface({ a: '1' }, undefined)).toEqual({ a: '1' })
  })

  it('treats a lower bound above the cap as 99+ too', () => {
    expect(inboxBadge({ total: 150, lower_bound: true, errors: [] }))
      .toEqual({ text: '99+', title: 'At least 150 items need you' })
  })
})
