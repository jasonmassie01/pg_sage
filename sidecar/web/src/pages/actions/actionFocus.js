// actionFocus: exact links into Actions. An Ask Sage citation of action:<id>
// links to #/actions?action=<id>: an executed change record (action log
// id), so a queued proposal that shares the number is never the focus.
// ux-01b adds tab=<key> (the tab to open) and queue=<id> (a queued
// proposal, focused on the Needs your review tab); the Inbox, ux-05b and
// ux-05e link with them. ux-03 adds the plans tab (plan=<id>, read by
// PlansTab). A value that is not exactly a known tab or a
// positive integer id is ignored.

export const ACTION_TABS = Object.freeze(
  ['plans', 'recommendations', 'pending', 'progress', 'executed', 'decisions'])

// A positive integer that fits a bigint: at most 19 digits, no leading 0.
const QUEUE_ID = /^[1-9][0-9]{0,18}$/
const BIGINT_MAX = '9223372036854775807'

function hashParams(hash) {
  return new URLSearchParams(String(hash || '').split('?')[1] || '')
}

// actionFocusID is the focused change record id from the hash, or ''.
export function actionFocusID(hash = window.location.hash) {
  return hashParams(hash).get('action') || ''
}

// actionTabParam is the tab= key when it names an Actions tab, or ''.
export function actionTabParam(hash = window.location.hash) {
  const tab = hashParams(hash).get('tab') || ''
  return ACTION_TABS.includes(tab) ? tab : ''
}

// queueFocusID is the queue= id of a queued proposal, or ''.
export function queueFocusID(hash = window.location.hash) {
  const id = hashParams(hash).get('queue') || ''
  if (!QUEUE_ID.test(id)) return ''
  return id.length === BIGINT_MAX.length && id > BIGINT_MAX ? '' : id
}

// isFocusedAction is true for the executed record with this id.
export function isFocusedAction(row, id) {
  return Boolean(id) && row?.record_kind !== 'queued' && String(row?.id) === id
}

// isFocusedQueue is true for the approval card of the queued proposal id.
export function isFocusedQueue(card, id) {
  return Boolean(id) && card != null && String(card.queue_id) === id
}
