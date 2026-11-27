import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { prefsAvailable, readPref, writePref } from './storage'

// dashboard-03: one guarded helper owns every dashboard preference read and
// write. Blocked site storage must degrade to defaults, never crash a render.
// No concurrent access tests: localStorage access is synchronous and the
// helper keeps no state of its own.

const securityError = () => new DOMException('storage blocked', 'SecurityError')
const storageDescriptor = Object.getOwnPropertyDescriptor(window, 'localStorage')

// Firefox with dom.storage.enabled=false exposes window.localStorage as null.
function nullStorage() {
  Object.defineProperty(window, 'localStorage', {
    configurable: true,
    get() { return null },
  })
}

function blockStorageAccessor(error) {
  Object.defineProperty(window, 'localStorage', {
    configurable: true,
    get() { throw error },
  })
}

beforeEach(() => { localStorage.clear() })
afterEach(() => {
  if (storageDescriptor) Object.defineProperty(window, 'localStorage', storageDescriptor)
  else delete window.localStorage
  vi.restoreAllMocks()
})

describe('readPref', () => {
  it('returns the stored value', () => {
    localStorage.setItem('pg_sage_range', '7d')
    expect(readPref('pg_sage_range', '24h')).toBe('7d')
  })

  it('returns the fallback for a missing key', () => {
    expect(readPref('pg_sage_missing', '24h')).toBe('24h')
  })

  it('returns the fallback for an empty stored value', () => {
    localStorage.setItem('pg_sage_db', '')
    expect(readPref('pg_sage_db', 'all')).toBe('all')
  })

  it('returns a null fallback unchanged when the key is missing', () => {
    expect(readPref('pg_sage_missing', null)).toBeNull()
  })

  it('returns undefined when the key is missing and no fallback is given', () => {
    expect(readPref('pg_sage_missing')).toBeUndefined()
  })

  it('returns the fallback when getItem throws SecurityError', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw securityError()
    })
    expect(readPref('pg_sage_db', 'all')).toBe('all')
  })

  // Chrome with site data blocked throws on the window.localStorage
  // accessor itself, before getItem is ever reached.
  it('returns the fallback when the localStorage accessor throws', () => {
    blockStorageAccessor(securityError())
    expect(readPref('pg_sage_db', 'all')).toBe('all')
  })

  it('returns the fallback when localStorage is null', () => {
    nullStorage()
    expect(readPref('pg_sage_db', 'all')).toBe('all')
  })

  it('rethrows an error that is not a DOMException', () => {
    const bug = new TypeError('getItem is not a function')
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw bug
    })
    expect(() => readPref('pg_sage_db', 'all')).toThrow(bug)
  })
})

describe('writePref', () => {
  it('stores the value and reports success', () => {
    expect(writePref('pg_sage_db', 'orders')).toBe(true)
    expect(localStorage.getItem('pg_sage_db')).toBe('orders')
  })

  it('stores non-string values as strings', () => {
    expect(writePref('pg_sage_numeric_pref', 1730851200000)).toBe(true)
    expect(localStorage.getItem('pg_sage_numeric_pref')).toBe('1730851200000')
  })

  it('overwrites an existing value', () => {
    localStorage.setItem('pg_sage_settings_mode', 'simple')
    writePref('pg_sage_settings_mode', 'advanced')
    expect(readPref('pg_sage_settings_mode', 'simple')).toBe('advanced')
  })

  it('reports failure when setItem throws QuotaExceededError', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('full', 'QuotaExceededError')
    })
    expect(writePref('pg_sage_db', 'orders')).toBe(false)
  })

  it('reports failure when the localStorage accessor throws', () => {
    blockStorageAccessor(securityError())
    expect(writePref('pg_sage_db', 'orders')).toBe(false)
  })

  it('reports failure when localStorage is null', () => {
    nullStorage()
    expect(writePref('pg_sage_db', 'orders')).toBe(false)
  })

  it('rethrows an error that is not a DOMException', () => {
    const bug = new RangeError('unexpected')
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw bug
    })
    expect(() => writePref('pg_sage_db', 'orders')).toThrow(bug)
  })
})

// ux-05c: the page guide asks whether a visit can be remembered at all,
// without writing anything.
describe('prefsAvailable', () => {
  it('reports true when localStorage answers, and writes nothing', () => {
    expect(prefsAvailable()).toBe(true)
    expect(localStorage.length).toBe(0)
  })

  it('reports false when the accessor throws SecurityError', () => {
    blockStorageAccessor(securityError())
    expect(prefsAvailable()).toBe(false)
  })

  it('reports false when localStorage is null', () => {
    nullStorage()
    expect(prefsAvailable()).toBe(false)
  })

  it('rethrows an error that is not a DOMException', () => {
    const bug = new TypeError('unexpected')
    blockStorageAccessor(bug)
    expect(() => prefsAvailable()).toThrow(bug)
  })
})
