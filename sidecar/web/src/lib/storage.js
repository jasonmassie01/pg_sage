// storage.js: the one place the dashboard touches localStorage.
//
// A browser with site data blocked throws SecurityError (a DOMException) from
// the window.localStorage accessor itself; a full store throws
// QuotaExceededError from setItem. Preferences are conveniences, so both fall
// back quietly. Any other error is a bug and is rethrown.

// readPref returns the stored string for key, or fallback when the key is
// missing, empty or storage is unavailable.
export function readPref(key, fallback) {
  try {
    const value = window.localStorage.getItem(key)
    return value === null || value === '' ? fallback : value
  } catch (err) {
    if (err instanceof DOMException) return fallback
    throw err
  }
}

// writePref stores value as a string and reports whether it was saved.
export function writePref(key, value) {
  try {
    window.localStorage.setItem(key, String(value))
    return true
  } catch (err) {
    if (err instanceof DOMException) return false
    throw err
  }
}
