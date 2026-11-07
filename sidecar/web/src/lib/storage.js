// storage.js: the one place the dashboard touches localStorage.
//
// A browser with site data blocked throws SecurityError (a DOMException) from
// the window.localStorage accessor itself; a full store throws
// QuotaExceededError from setItem; Firefox with dom.storage.enabled=false
// exposes localStorage as null. Preferences are conveniences, so all three
// fall back quietly. Any other error is a bug and is rethrown.

// readPref returns the stored string for key, or fallback when the key is
// missing, empty or storage is unavailable.
export function readPref(key, fallback) {
  try {
    const store = window.localStorage
    if (!store) return fallback
    const value = store.getItem(key)
    return value === null || value === '' ? fallback : value
  } catch (err) {
    if (err instanceof DOMException) return fallback
    throw err
  }
}

// writePref stores value as a string and reports whether it was saved.
export function writePref(key, value) {
  try {
    const store = window.localStorage
    if (!store) return false
    store.setItem(key, String(value))
    return true
  } catch (err) {
    if (err instanceof DOMException) return false
    throw err
  }
}
