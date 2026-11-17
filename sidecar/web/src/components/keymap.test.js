import { describe, expect, it } from 'vitest'
import { APPROVAL_KEYMAP, KEYMAP, approvalShortcutFor, isTyping, shortcutFor } from './keymap'

// ux-05a win 24: the approval list's verbs live in the same table the
// shortcuts sheet renders, so the sheet and the handler cannot drift.
// No concurrent access tests: pure functions over a frozen table.

const key = (k, init = {}) => ({ key: k, target: document.body, ...init })

describe('APPROVAL_KEYMAP', () => {
  it('names next, previous, approve, reject and snooze in that order', () => {
    expect(APPROVAL_KEYMAP.map(e => e.id))
      .toEqual(['next', 'previous', 'approve', 'reject', 'snooze'])
    expect(APPROVAL_KEYMAP.map(e => e.keys)).toEqual([['j'], ['k'], ['a'], ['r'], ['s']])
    for (const entry of APPROVAL_KEYMAP) expect(entry.description.length).toBeGreaterThan(10)
    expect(Object.isFrozen(APPROVAL_KEYMAP)).toBe(true)
  })

  it('maps each verb key to its entry', () => {
    expect(['j', 'k', 'a', 'r', 's'].map(k => approvalShortcutFor(key(k))))
      .toEqual(['next', 'previous', 'approve', 'reject', 'snooze'])
  })

  it('says the approve and reject verbs ask before they decide', () => {
    const byId = Object.fromEntries(APPROVAL_KEYMAP.map(e => [e.id, e.description]))
    expect(byId.approve).toMatch(/confirm/i)
    expect(byId.reject).toMatch(/reason/i)
  })

  it('stays out of the global keymap the palette matches', () => {
    expect(KEYMAP.map(e => e.id)).toEqual(['palette', 'shortcuts', 'close', 'move', 'run'])
    expect(shortcutFor(key('j'))).toBeUndefined()
    expect(shortcutFor(key('a'))).toBeUndefined()
  })
})

describe('approvalShortcutFor ignores keys that are not approval verbs', () => {
  it('returns nothing for other letters, capitals and empty events', () => {
    expect(approvalShortcutFor(key('x'))).toBeUndefined()
    expect(approvalShortcutFor(key('A'))).toBeUndefined()
    expect(approvalShortcutFor(key('J', { shiftKey: true }))).toBeUndefined()
    expect(approvalShortcutFor({})).toBeUndefined()
    expect(approvalShortcutFor(null)).toBeUndefined()
  })

  it('returns nothing with Ctrl, Cmd or Alt held', () => {
    expect(approvalShortcutFor(key('a', { ctrlKey: true }))).toBeUndefined()
    expect(approvalShortcutFor(key('a', { metaKey: true }))).toBeUndefined()
    expect(approvalShortcutFor(key('r', { altKey: true }))).toBeUndefined()
  })

  it('returns nothing while typing in a field', () => {
    for (const tag of ['input', 'textarea', 'select']) {
      const el = document.createElement(tag)
      expect(approvalShortcutFor(key('a', { target: el })), tag).toBeUndefined()
    }
    const editable = document.createElement('div')
    editable.setAttribute('contenteditable', 'true') // jsdom has no contentEditable IDL
    expect(isTyping(editable)).toBe(true)
    expect(approvalShortcutFor(key('a', { target: editable }))).toBeUndefined()
  })
})

describe('isTyping', () => {
  it('is false for buttons, the body and no element', () => {
    expect(isTyping(document.createElement('button'))).toBe(false)
    expect(isTyping(document.body)).toBe(false)
    expect(isTyping(null)).toBe(false)
    expect(isTyping(undefined)).toBe(false)
  })

  it('is true for text fields', () => {
    expect(isTyping(document.createElement('input'))).toBe(true)
    expect(isTyping(document.createElement('textarea'))).toBe(true)
  })
})
