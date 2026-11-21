import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import App from './App'

// ux-10b: the header scope is "all", a tag scope (?tag=, read by the pages
// that support it) or one database (?database= links). A link naming a
// database the user cannot see falls back to "all" with a notice. Pages are
// stubbed to report the scope they were given. No concurrent access tests:
// the picker's request races are covered in DatabasePicker.test.jsx.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))
vi.mock('./hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))
vi.mock('./hooks/useLiveEvents', () => ({
  LiveEventsProvider: ({ children }) => children,
  useLiveRefetch: vi.fn(),
}))
vi.mock('./components/Toast', () => ({
  ToastProvider: ({ children }) => children,
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn() }),
}))
vi.mock('./pages/CasesPage', () => ({
  CasesPage: ({ database }) => <div data-testid="cases-page">cases:{database}</div>,
}))
vi.mock('./pages/ValuePage', () => ({
  ValuePage: ({ database }) => <div data-testid="value-page">value:{database}</div>,
}))
vi.mock('./pages/DatabasesPage', () => ({
  DatabasesPage: ({ scopeTag }) => <div data-testid="fleet-page">fleet:{scopeTag || '-'}</div>,
}))
vi.mock('./pages/InboxPage', () => ({
  InboxPage: ({ database }) => <div data-testid="inbox-page">inbox:{database}</div>,
}))
vi.mock('./components/onboarding/OnboardingPanel', () => ({ OnboardingPanel: () => null }))
vi.mock('./components/onboarding/FirstLookPanel', () => ({ FirstLookPanel: () => null }))

const names = ['orders', 'billing', 'payments', 'payroll', 'search', 'reports', 'audit', 'ml']
// The Fleet page is admin-only, so the scope walk signs in as an admin.
const me = { role: 'admin' }
let fleet = { summary: {}, databases: names.map((name, i) => ({ id: i + 1, name,
  status: { trust_level: 'advisory' } })) }

function mockFetch() {
  globalThis.fetch = vi.fn(async url => {
    if (url === '/api/v1/auth/mode') {
      return { ok: true, status: 200, json: async () => ({ mode: 'session' }) }
    }
    if (url === '/api/v1/auth/me') {
      return { ok: true, status: 200,
        json: async () => ({ email: 'admin@example.com', role: me.role }) }
    }
    if (String(url).startsWith('/api/v1/databases?')) {
      const q = new URL(url, 'http://x').searchParams.get('q') || ''
      const rows = names.filter(n => n.includes(q)).map(name => ({ name }))
      return { ok: true, status: 200, json: async () => ({ databases: rows, total: rows.length,
        next_cursor: '' }) }
    }
    return { ok: false, status: 404, json: async () => null, text: async () => '' }
  })
}

beforeEach(() => {
  me.role = 'admin'
  localStorage.clear()
  fleet = { summary: {}, databases: names.map((name, i) => ({ id: i + 1, name,
    status: { trust_level: 'advisory' } })) }
  api.useAPI.mockReset()
  api.useAPI.mockImplementation(url => ({ data: url === '/api/v1/databases' ? fleet : null,
    loading: false, error: null, refetch: vi.fn() }))
  mockFetch()
})
afterEach(() => {
  window.location.hash = ''
  vi.restoreAllMocks()
})

async function open(hash) {
  window.location.hash = hash
  render(<App />)
  await screen.findByTestId('app-loaded')
}

const scope = () => screen.getByRole('combobox', { name: 'Database scope' })
const go = hash => act(() => {
  window.location.hash = hash
  window.dispatchEvent(new HashChangeEvent('hashchange'))
})

describe('App scope links', () => {
  it('falls back to all with a notice for a database the user cannot see', async () => {
    localStorage.setItem('pg_sage_db', 'orders')
    await open('#/cases?database=ghost')
    expect(screen.getByTestId('cases-page')).toHaveTextContent('cases:all')
    expect(screen.getByTestId('scope-notice')).toHaveTextContent(
      'ghost is not a database you can see. Showing all databases.')
    expect(scope()).toHaveValue('All databases')
  })

  it('makes no claim while the inventory is still loading', async () => {
    api.useAPI.mockImplementation(() => ({ data: null, loading: true, error: null,
      refetch: vi.fn() }))
    await open('#/cases?database=ghost')
    expect(screen.queryByTestId('scope-notice')).toBeNull()
  })

  it('opens a visible linked database without a notice', async () => {
    await open('#/cases?database=billing')
    expect(screen.getByTestId('cases-page')).toHaveTextContent('cases:billing')
    expect(screen.queryByTestId('scope-notice')).toBeNull()
  })
})

describe('App scope transitions', () => {
  it('goes all, tag, one database and back to all with no leftovers', async () => {
    await open('#/manage-databases')
    expect(screen.getByTestId('fleet-page')).toHaveTextContent('fleet:-')

    // A tag scope: Fleet reads it; a page without tag support says so.
    fireEvent.focus(scope())
    fireEvent.change(scope(), { target: { value: 'tag:tier=prod' } })
    fireEvent.click(await screen.findByRole('option',
      { name: 'All databases tagged tier=prod' }))
    await waitFor(() => expect(window.location.hash).toBe('#/manage-databases?tag=tier%3Dprod'))
    expect(screen.getByTestId('fleet-page')).toHaveTextContent('fleet:tier=prod')
    expect(scope()).toHaveValue('tag:tier=prod')
    // ux-11: #/value redirects to the ledger's Value section, tag kept.
    go('#/value?tag=tier%3Dprod')
    expect(screen.getByTestId('needs-one-database')).toHaveTextContent(
      'This page needs one database')
    expect(screen.queryByTestId('value-page')).toBeNull()

    // One database: the tag leaves the hash and the page renders it.
    fireEvent.focus(scope())
    fireEvent.change(scope(), { target: { value: 'payroll' } })
    await waitFor(() => expect(screen.getAllByRole('option').map(o => o.textContent))
      .toContain('payroll'))
    fireEvent.keyDown(scope(), { key: 'Enter' })
    await waitFor(() => expect(screen.getByTestId('value-page')).toHaveTextContent(
      'value:payroll'))
    expect(window.location.hash).toBe('#/ledger?section=value')
    expect(screen.queryByTestId('needs-one-database')).toBeNull()

    // Back to all.
    fireEvent.focus(scope())
    fireEvent.click(await screen.findByRole('option', { name: 'All databases' }))
    await waitFor(() => expect(screen.getByTestId('value-page')).toHaveTextContent(
      'value:all'))
    expect(scope()).toHaveValue('All databases')
    expect(screen.queryByTestId('needs-one-database')).toBeNull()
    expect(screen.queryByTestId('scope-notice')).toBeNull()
  })

  it('clears a tag scope from the needs-one-database notice', async () => {
    await open('#/value?tag=tier%3Dprod')
    fireEvent.click(screen.getByRole('button', { name: 'Show all databases' }))
    await waitFor(() => expect(screen.getByTestId('value-page')).toHaveTextContent(
      'value:all'))
    expect(window.location.hash).toBe('#/ledger?section=value')
  })

  it('says no databases yet with the add-database link for an empty fleet', async () => {
    fleet = { summary: {}, databases: [] }
    await open('#/value')
    expect(screen.queryByRole('combobox', { name: 'Database scope' })).toBeNull()
    const empty = screen.getByTestId('scope-empty')
    expect(empty).toHaveTextContent('No databases yet')
    expect(empty.querySelector('a')).toHaveAttribute('href', '#/manage-databases')
  })

  it('asks a non-admin to have a database added, with no link they cannot open', async () => {
    me.role = 'viewer'
    fleet = { summary: {}, databases: [] }
    await open('#/value')
    const empty = screen.getByTestId('scope-empty')
    expect(empty).toHaveTextContent('No databases yet. Ask an admin to add one.')
    expect(empty.querySelector('a')).toBeNull()
  })

  it('links the tagged databases only for an admin', async () => {
    me.role = 'operator'
    await open('#/value?tag=tier%3Dprod')
    expect(screen.getByTestId('needs-one-database')).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'List the tagged databases' })).toBeNull()
  })
})
