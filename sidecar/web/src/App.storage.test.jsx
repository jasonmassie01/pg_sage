import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

// dashboard-03: with site storage blocked the browser throws SecurityError
// from localStorage. The login screen and the signed-in shell must still
// render, and switching databases must not crash when writes fail.
// No concurrent access tests: render-time, single-threaded UI state with
// no shared writers.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))

vi.mock('./hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
}))
vi.mock('./hooks/useLiveEvents', () => ({
  LiveEventsProvider: ({ children }) => children,
  useLiveRefetch: vi.fn(),
}))
vi.mock('./components/Toast', () => ({
  ToastProvider: ({ children }) => children,
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }),
}))

const fleet = {
  summary: { emergency_stopped: false },
  databases: [
    { id: 7, name: 'orders', status: { trust_level: 'advisory' } },
    { id: 8, name: 'billing', status: { trust_level: 'advisory' } },
  ],
}

const securityError = () => new DOMException('storage blocked', 'SecurityError')
const storageDescriptor = Object.getOwnPropertyDescriptor(window, 'localStorage')

function blockStorageAccessor() {
  Object.defineProperty(window, 'localStorage', {
    configurable: true,
    get() { throw securityError() },
  })
}

function mockFetch({ signedIn }) {
  globalThis.fetch = vi.fn(async url => {
    if (url === '/api/v1/auth/mode') {
      return { ok: true, status: 200, json: async () => ({ mode: 'session' }) }
    }
    if (url === '/api/v1/auth/me') {
      return signedIn
        ? { ok: true, status: 200,
          json: async () => ({ email: 'op@example.com', role: 'operator' }) }
        : { ok: false, status: 401, json: async () => ({ error: 'no session' }) }
    }
    return { ok: false, status: 404, json: async () => null }
  })
}

beforeEach(() => {
  localStorage.clear()
  window.location.hash = '#/slos'
  api.useAPI.mockReset()
  api.useAPI.mockImplementation(url => ({
    data: url === '/api/v1/databases' ? fleet : null,
    loading: false,
    error: null,
    refetch: vi.fn(),
  }))
})

afterEach(() => {
  if (storageDescriptor) Object.defineProperty(window, 'localStorage', storageDescriptor)
  else delete window.localStorage
  window.location.hash = ''
  vi.restoreAllMocks()
})

describe('App with blocked site storage: login screen', () => {
  it('renders the login screen when getItem throws SecurityError', async () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw securityError()
    })
    mockFetch({ signedIn: false })
    render(<App />)

    expect(await screen.findByTestId('login-email')).toBeInTheDocument()
    expect(screen.getByTestId('login-submit')).toBeInTheDocument()
  })

  it('renders the login screen when the localStorage accessor throws', async () => {
    blockStorageAccessor()
    mockFetch({ signedIn: false })
    render(<App />)

    expect(await screen.findByTestId('login-email')).toBeInTheDocument()
  })

  // Firefox with dom.storage.enabled=false exposes window.localStorage as null.
  it('renders the login screen when localStorage is null', async () => {
    Object.defineProperty(window, 'localStorage', {
      configurable: true,
      get() { return null },
    })
    mockFetch({ signedIn: false })
    render(<App />)

    expect(await screen.findByTestId('login-email')).toBeInTheDocument()
  })
})

describe('App with blocked site storage: signed in', () => {
  it('renders the shell with default preferences when storage is blocked', async () => {
    blockStorageAccessor()
    mockFetch({ signedIn: true })
    render(<App />)

    await screen.findByTestId('app-loaded')
    expect(screen.getByTestId('database-picker')).toHaveValue('all')
    expect(screen.getByTestId('header-emergency-stop'))
      .toHaveTextContent('Stop all 2 databases')
  })

  it('switches databases when setItem throws SecurityError', async () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
      .mockImplementation(() => { throw securityError() })
    mockFetch({ signedIn: true })
    render(<App />)
    await screen.findByTestId('app-loaded')

    fireEvent.change(screen.getByTestId('database-picker'),
      { target: { value: 'billing' } })

    expect(screen.getByTestId('database-picker')).toHaveValue('billing')
    expect(screen.getByTestId('header-emergency-stop'))
      .toHaveTextContent('Stop billing')
    expect(setItem).toHaveBeenCalledWith('pg_sage_db', 'billing')
  })

  it('switches databases when setItem throws QuotaExceededError', async () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('full', 'QuotaExceededError')
    })
    mockFetch({ signedIn: true })
    render(<App />)
    await screen.findByTestId('app-loaded')

    fireEvent.change(screen.getByTestId('database-picker'),
      { target: { value: 'orders' } })

    expect(screen.getByTestId('database-picker')).toHaveValue('orders')
  })

  it('restores the stored database when storage works', async () => {
    localStorage.setItem('pg_sage_db', 'billing')
    mockFetch({ signedIn: true })
    render(<App />)
    await screen.findByTestId('app-loaded')

    expect(screen.getByTestId('database-picker')).toHaveValue('billing')
    fireEvent.change(screen.getByTestId('database-picker'),
      { target: { value: 'orders' } })
    expect(localStorage.getItem('pg_sage_db')).toBe('orders')
  })

  it('falls back to all databases when the stored name is empty', async () => {
    localStorage.setItem('pg_sage_db', '')
    mockFetch({ signedIn: true })
    render(<App />)
    await screen.findByTestId('app-loaded')

    expect(screen.getByTestId('database-picker')).toHaveValue('all')
  })
})
