// keymap.js: the dashboard keyboard shortcuts (dashboard-06). The command
// palette's key handlers match keys with these entries, and the shortcuts
// sheet renders its rows from the same list, so the two cannot drift.

const isMac = () =>
  typeof navigator !== 'undefined' && /mac/i.test(navigator.platform || '')

export const PALETTE_KEY = isMac() ? '⌘K' : 'Ctrl+K'

export const KEYMAP = Object.freeze([
  { id: 'palette', keys: [PALETTE_KEY], description: 'Open or close the command palette',
    matches: e => (e.metaKey || e.ctrlKey) && e.key?.toLowerCase() === 'k' },
  { id: 'shortcuts', keys: ['?'], description: 'Show these keyboard shortcuts',
    matches: e => e.key === '?' },
  { id: 'close', keys: ['Esc'], description: 'Close the palette or this sheet',
    matches: e => e.key === 'Escape' },
  { id: 'move', keys: ['↑', '↓'], description: 'Move through palette results',
    matches: e => e.key === 'ArrowUp' || e.key === 'ArrowDown' },
  { id: 'run', keys: ['Enter'], description: 'Run the selected command',
    matches: e => e.key === 'Enter' },
])

// shortcutFor returns the id of the keymap entry the key event matches.
export function shortcutFor(event) {
  return KEYMAP.find(entry => entry.matches(event))?.id
}

// isTyping is true when el takes text, so a letter key types instead of
// running a shortcut.
export function isTyping(el) {
  if (!el) return false
  const tag = (el.tagName || '').toLowerCase()
  return tag === 'input' || tag === 'textarea' || tag === 'select'
    || el.isContentEditable === true || el.getAttribute?.('contenteditable') === 'true'
}

const plainKey = (event, key) => event.key === key
  && !event.ctrlKey && !event.metaKey && !event.altKey

const CONTROL_TAGS = new Set(['button', 'a', 'summary', 'input', 'select', 'textarea'])
const CONTROL_ROLES = new Set(['button', 'link', 'checkbox', 'menuitem', 'option', 'radio',
  'switch', 'tab'])

// isInteractive is true when el is a control Enter already acts on (a
// button, link, checkbox and the like), so the list's Enter leaves it be.
export function isInteractive(el) {
  if (!el || el === document.body) return false
  const tag = (el.tagName || '').toLowerCase()
  return CONTROL_TAGS.has(tag) || CONTROL_ROLES.has(el.getAttribute?.('role') || '')
    || isTyping(el)
}

// APPROVAL_KEYMAP holds the approval list's verbs (ux-05a; x and Enter,
// ux-07b). They act only on an approval list (the Waiting on you cards,
// the Inbox), never while typing; approve and reject still ask (a confirm,
// a reason) before anything is sent.
export const APPROVAL_KEYMAP = Object.freeze([
  { id: 'next', keys: ['j'], description: 'Select the next approval card',
    matches: e => plainKey(e, 'j') },
  { id: 'previous', keys: ['k'], description: 'Select the previous approval card',
    matches: e => plainKey(e, 'k') },
  { id: 'approve', keys: ['a'],
    description: 'Approve the selected card, or the cards marked with x (asks you to confirm)',
    matches: e => plainKey(e, 'a') },
  { id: 'reject', keys: ['r'], description: 'Reject the selected card (opens the reason field)',
    matches: e => plainKey(e, 'r') },
  { id: 'snooze', keys: ['s'], description: 'Open the snooze options of the selected card',
    matches: e => plainKey(e, 's') },
  { id: 'toggle', keys: ['x'],
    description: 'Mark or unmark the selected card for bulk approve (safe tier only)',
    matches: e => plainKey(e, 'x') },
  { id: 'open', keys: ['Enter'], description: 'Open the selected card',
    matches: e => plainKey(e, 'Enter') && !e.shiftKey && !isInteractive(e.target) },
])

// approvalShortcutFor returns the id of the approval verb the key event
// names, or undefined while typing or with a modifier held.
export function approvalShortcutFor(event) {
  if (!event || isTyping(event.target)) return undefined
  return APPROVAL_KEYMAP.find(entry => entry.matches(event))?.id
}
