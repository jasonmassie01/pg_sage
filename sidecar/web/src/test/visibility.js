// Page Visibility doubles for jsdom, whose document is always visible.
// hideTab and showTab set document.hidden and visibilityState the way a
// browser does when the operator switches tabs, and fire visibilitychange.
// resetVisibility restores jsdom's own getters; call it in afterEach.

function setHidden(hidden) {
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden })
  Object.defineProperty(document, 'visibilityState', {
    configurable: true, get: () => (hidden ? 'hidden' : 'visible'),
  })
}

export function hideTab() {
  setHidden(true)
  document.dispatchEvent(new Event('visibilitychange'))
}

export function showTab() {
  setHidden(false)
  document.dispatchEvent(new Event('visibilitychange'))
}

export function resetVisibility() {
  delete document.hidden
  delete document.visibilityState
}
