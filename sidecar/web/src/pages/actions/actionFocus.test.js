import { describe, expect, it } from 'vitest'
import { actionFocusID, actionTabParam, isFocusedQueue, queueFocusID } from './actionFocus'

// ux-01b: exact links into Actions. tab= opens a tab, queue= focuses a
// queued proposal (distinct from action=, an executed action-log id).
// Hostile or malformed values are ignored, never echoed into the page.
// No concurrent access tests: pure functions of the hash.

describe('actionTabParam', () => {
  it('reads a known tab key', () => {
    expect(actionTabParam('#/actions?tab=pending&queue=42&database=orders')).toBe('pending')
    for (const key of ['recommendations', 'pending', 'progress', 'executed', 'decisions']) {
      expect(actionTabParam(`#/actions?tab=${key}`)).toBe(key)
    }
  })

  it('ignores unknown, hostile, empty and differently cased keys', () => {
    for (const hash of ['#/actions?tab=plans', '#/actions?tab=%3Cscript%3E',
      '#/actions?tab=', '#/actions', '', '#/actions?tab=PENDING',
      '#/actions?tab=pending%20', '#/actions?tab=__proto__', undefined, null]) {
      expect(actionTabParam(hash)).toBe('')
    }
  })
})

describe('queueFocusID', () => {
  it('reads a positive integer queue id', () => {
    expect(queueFocusID('#/actions?tab=pending&queue=42&database=orders')).toBe('42')
    expect(queueFocusID('#/actions?queue=9223372036854775807')).toBe('9223372036854775807')
  })

  it('ignores hostile and malformed ids', () => {
    for (const raw of ['42abc', '-1', '0', '1e3', '%2042', '4.2', '<img src=x onerror=1>',
      '12345678901234567890', '', '042x']) {
      expect(queueFocusID(`#/actions?queue=${raw}`)).toBe('')
    }
    expect(queueFocusID('')).toBe('')
    expect(queueFocusID(undefined)).toBe('')
  })

  it('keeps queue= and action= apart', () => {
    expect(queueFocusID('#/actions?action=42')).toBe('')
    expect(actionFocusID('#/actions?queue=42')).toBe('')
    expect(actionFocusID('#/actions?action=7&queue=42')).toBe('7')
    expect(queueFocusID('#/actions?action=7&queue=42')).toBe('42')
  })
})

describe('isFocusedQueue', () => {
  it('matches the queued card with that id only', () => {
    expect(isFocusedQueue({ queue_id: 42 }, '42')).toBe(true)
    expect(isFocusedQueue({ queue_id: '42' }, '42')).toBe(true)
    expect(isFocusedQueue({ queue_id: 420 }, '42')).toBe(false)
    expect(isFocusedQueue({ queue_id: 42 }, '')).toBe(false)
    expect(isFocusedQueue(null, '42')).toBe(false)
  })
})
