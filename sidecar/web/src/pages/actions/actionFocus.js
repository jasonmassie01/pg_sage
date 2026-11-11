// actionFocus: an Ask Sage citation of action:<id> links to
// #/actions?action=<id>. The id is an executed change record (action log
// id), so a queued proposal that shares the number is never the focus.

// actionFocusID is the focused change record id from the hash, or ''.
export function actionFocusID(hash = window.location.hash) {
  const query = String(hash || '').split('?')[1] || ''
  return new URLSearchParams(query).get('action') || ''
}

// isFocusedAction is true for the executed record with this id.
export function isFocusedAction(row, id) {
  return Boolean(id) && row?.record_kind !== 'queued' && String(row?.id) === id
}
