import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ProfilePage } from './ProfilePage'

// apiauth-03: a password user changes their own password from the account
// page; an SSO-only user sees no form; the server's refusal text is shown.
// No concurrent access tests: single-threaded UI state.

const user = { id: 3, email: 'me@x.test', role: 'operator', password_login: true }

function ok(body) {
  return { ok: true, status: 200, json: async () => body }
}

function fail(status, error) {
  return { ok: false, status, json: async () => ({ error }) }
}

function fill(current, next, confirm = next) {
  fireEvent.change(screen.getByTestId('current-password'), { target: { value: current } })
  fireEvent.change(screen.getByTestId('new-password'), { target: { value: next } })
  fireEvent.change(screen.getByTestId('confirm-password'), { target: { value: confirm } })
  fireEvent.click(screen.getByTestId('change-password-submit'))
}

function passwordCalls() {
  return globalThis.fetch.mock.calls.filter(([url]) => url === '/api/v1/auth/password')
}

describe('ProfilePage password change', () => {
  let changeResponse
  beforeEach(() => {
    window.location.hash = '#/profile'
    changeResponse = ok({ status: 'changed', sessions_ended: 2 })
    globalThis.fetch = vi.fn(url => {
      if (url === '/api/v1/auth/password') return Promise.resolve(changeResponse)
      return Promise.resolve(ok({ linked: false, oauth_enabled: false }))
    })
  })
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('sends the current and new password and reports success', async () => {
    render(<ProfilePage user={user} navigate={vi.fn()} />)
    fill('old-password-1', 'new-password-12')
    expect(await screen.findByTestId('change-password-success'))
      .toHaveTextContent('2 other sessions')
    expect(passwordCalls()).toHaveLength(1)
    const [, init] = passwordCalls()[0]
    expect(init).toMatchObject({ method: 'PUT', credentials: 'include' })
    expect(init.headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(init.body)).toEqual({ current_password: 'old-password-1',
      new_password: 'new-password-12' })
    expect(screen.getByTestId('new-password')).toHaveValue('')
  })

  it('refuses a short, mismatched or email password before calling the server', async () => {
    render(<ProfilePage user={user} navigate={vi.fn()} />)
    fill('old-password-1', 'eleven-byte')
    expect(await screen.findByTestId('change-password-error')).toHaveTextContent('12')
    fill('old-password-1', 'new-password-12', 'new-password-13')
    expect(screen.getByTestId('change-password-error')).toHaveTextContent('match')
    fill('old-password-1', 'ME@x.test   ')
    expect(screen.getByTestId('change-password-error')).toHaveTextContent('email')
    fill('old-password-1', 'x'.repeat(73))
    expect(screen.getByTestId('change-password-error')).toHaveTextContent('72')
    expect(passwordCalls()).toHaveLength(0)
  })

  it('shows the server error for a wrong current password and keeps the fields', async () => {
    changeResponse = fail(401, 'current password is incorrect')
    render(<ProfilePage user={user} navigate={vi.fn()} />)
    fill('wrong-password', 'new-password-12')
    expect(await screen.findByTestId('change-password-error'))
      .toHaveTextContent('current password is incorrect')
    expect(screen.queryByTestId('change-password-success')).toBeNull()
    expect(screen.getByTestId('current-password')).toHaveValue('wrong-password')
  })

  it('hides the form for an SSO-only account', async () => {
    render(<ProfilePage user={{ ...user, password_login: false }} navigate={vi.fn()} />)
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
    expect(screen.queryByTestId('change-password-form')).toBeNull()
  })

  it('hides the form in Databricks Apps mode', () => {
    render(<ProfilePage user={{ ...user, auth_mode: 'databricks_app' }} />)
    expect(screen.queryByTestId('change-password-form')).toBeNull()
  })
})
