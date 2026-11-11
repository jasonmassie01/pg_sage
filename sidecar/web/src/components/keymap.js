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
