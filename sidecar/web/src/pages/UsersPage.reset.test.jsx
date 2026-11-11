import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { UsersPage } from './UsersPage'

// apiauth-03: an admin resets a password user's password after confirming;
// the temporary password is shown once with a copy button. SSO-only users
// and the signed-in admin's own row offer no reset.
// No concurrent access tests: single-threaded UI state.

const base = { created_at: '2026-09-01T00:00:00Z', last_login: null, sso_linked: false,
  sso_issuer: '', password_login: true, disabled: false }
const users = [
  { ...base, id: 1, email: 'admin@x.test', role: 'admin' },
  { ...base, id: 5, email: 'viewer@x.test', role: 'viewer' },
  { ...base, id: 6, email: 'sso@x.test', role: 'viewer', password_login: false },
]
const temp = 'TempPassword-abcdefghijklmnopq'

function ok(body) {
  return { ok: true, status: 200, json: async () => body }
}

function rowFor(email) {
  return screen.getByText(email).closest('tr')
}

function resetCalls() {
  return globalThis.fetch.mock.calls.filter(([url]) => url.endsWith('/password-reset'))
}

describe('UsersPage password reset', () => {
  let resetResponse
  beforeEach(() => {
    resetResponse = ok({ temporary_password: temp, must_change_password: true,
      sessions_ended: 1 })
    globalThis.fetch = vi.fn(url => {
      if (url === '/api/v1/users') return Promise.resolve(ok({ users }))
      return Promise.resolve(resetResponse)
    })
    globalThis.confirm = vi.fn(() => true)
    Object.defineProperty(globalThis.navigator, 'clipboard', {
      value: { writeText: vi.fn(() => Promise.resolve()) }, configurable: true })
  })
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('resets after confirmation and shows the temporary password once', async () => {
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('viewer@x.test')
    fireEvent.click(within(rowFor('viewer@x.test')).getByTestId('user-reset-password-button'))
    expect(globalThis.confirm).toHaveBeenCalledWith(expect.stringContaining('viewer@x.test'))
    const notice = await screen.findByTestId('temp-password-notice')
    expect(within(notice).getByTestId('temp-password-value')).toHaveTextContent(temp)
    expect(notice).toHaveTextContent('viewer@x.test')
    expect(resetCalls()).toHaveLength(1)
    expect(resetCalls()[0][0]).toBe('/api/v1/users/5/password-reset')
    expect(resetCalls()[0][1]).toMatchObject({ method: 'POST', credentials: 'include' })
    fireEvent.click(within(notice).getByTestId('temp-password-copy'))
    await waitFor(() => expect(navigator.clipboard.writeText).toHaveBeenCalledWith(temp))
    fireEvent.click(within(notice).getByText('Dismiss'))
    expect(screen.queryByTestId('temp-password-notice')).toBeNull()
    expect(screen.queryByText(temp)).toBeNull()
  })

  it('does nothing when the confirmation is declined', async () => {
    globalThis.confirm = vi.fn(() => false)
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('viewer@x.test')
    fireEvent.click(within(rowFor('viewer@x.test')).getByTestId('user-reset-password-button'))
    expect(resetCalls()).toHaveLength(0)
    expect(screen.queryByTestId('temp-password-notice')).toBeNull()
  })

  it('shows the server error and no temporary password on failure', async () => {
    resetResponse = { ok: false, status: 409,
      json: async () => ({ error: 'no password to reset: this account signs in with SSO' }) }
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('viewer@x.test')
    fireEvent.click(within(rowFor('viewer@x.test')).getByTestId('user-reset-password-button'))
    expect(await screen.findByText(/no password to reset/)).toBeInTheDocument()
    expect(screen.queryByTestId('temp-password-notice')).toBeNull()
  })

  it('offers no reset for SSO-only users or the signed-in admin', async () => {
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('sso@x.test')
    expect(within(rowFor('sso@x.test')).queryByTestId('user-reset-password-button')).toBeNull()
    expect(within(rowFor('admin@x.test')).queryByTestId('user-reset-password-button'))
      .toBeNull()
  })

  it('offers no reset in Databricks Apps mode', async () => {
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} authMode="databricks_app" />)
    await screen.findByText('viewer@x.test')
    expect(screen.queryByTestId('user-reset-password-button')).toBeNull()
  })
})
