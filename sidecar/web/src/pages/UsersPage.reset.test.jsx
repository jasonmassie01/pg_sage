import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { UsersPage } from './UsersPage'
import { answerConfirm } from '../test/confirmDialog'

// apiauth-03: an admin resets a password user's password after confirming;
// the temporary password is shown once with a copy button. SSO-only users
// and the signed-in admin's own row offer no reset. dashboard-10b: the
// confirmation is the shared ConfirmDialog, not window.confirm.
// Concurrency: the in-flight double click below; no other shared state.

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
    expect(await answerConfirm()).toContain('viewer@x.test')
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
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('viewer@x.test')
    fireEvent.click(within(rowFor('viewer@x.test')).getByTestId('user-reset-password-button'))
    await answerConfirm('cancel')
    expect(resetCalls()).toHaveLength(0)
    expect(screen.queryByTestId('temp-password-notice')).toBeNull()
  })

  it('shows the server error and no temporary password on failure', async () => {
    resetResponse = { ok: false, status: 409,
      json: async () => ({ error: 'no password to reset: this account signs in with SSO' }) }
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('viewer@x.test')
    fireEvent.click(within(rowFor('viewer@x.test')).getByTestId('user-reset-password-button'))
    await answerConfirm()
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

describe('UsersPage password reset dialog (dashboard-10b)', () => {
  const evil = '<script>alert(1)</script>@x.test'
  const rows = [
    { ...base, id: 1, email: 'admin@x.test', role: 'admin' },
    { ...base, id: 5, email: 'viewer@x.test', role: 'viewer' },
    { ...base, id: 7, email: 'ops@x.test', role: 'operator' },
    { ...base, id: 9, email: evil, role: 'viewer' },
  ]
  let reply
  beforeEach(() => {
    reply = () => Promise.resolve(ok({ temporary_password: temp }))
    globalThis.fetch = vi.fn(url => {
      if (url === '/api/v1/users') return Promise.resolve(ok({ users: rows }))
      return reply()
    })
  })
  afterEach(() => vi.restoreAllMocks())

  async function openFor(email) {
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText(email)
    fireEvent.click(within(rowFor(email)).getByTestId('user-reset-password-button'))
    return screen.findByRole('dialog')
  }

  it('names the user and sends exactly the recorded request on Confirm', async () => {
    const dialog = await openFor('viewer@x.test')
    expect(within(dialog).getByRole('heading'))
      .toHaveTextContent('Reset password of viewer@x.test')
    expect(dialog).toHaveTextContent(/signed out everywhere/)
    expect(dialog).toHaveTextContent(/new password at their next sign-in/)
    expect(within(dialog).getByTestId('confirm-dialog-confirm'))
      .toHaveTextContent('Reset password')
    expect(within(dialog).queryByTestId('confirm-dialog-risk')).toBeNull()
    expect(resetCalls()).toHaveLength(0)
    await answerConfirm()
    await screen.findByTestId('temp-password-notice')
    expect(resetCalls()).toEqual([['/api/v1/users/5/password-reset', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      credentials: 'include', body: '{}',
    }]])
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  it('shows an email holding markup as text in the title', async () => {
    const dialog = await openFor(evil)
    expect(within(dialog).getByRole('heading').textContent)
      .toBe(`Reset password of ${evil}`)
    expect(dialog.querySelector('script')).toBeNull()
    await answerConfirm()
    await screen.findByTestId('temp-password-notice')
    expect(resetCalls()[0][0]).toBe('/api/v1/users/9/password-reset')
  })

  it.each([
    ['a 500 with a message', () => Promise.resolve({ ok: false, status: 500,
      json: async () => ({ error: 'store unavailable' }) }), 'store unavailable'],
    ['a non-JSON body', () => Promise.resolve({ ok: false, status: 502,
      json: async () => { throw new SyntaxError('Unexpected token <') } }),
    'Failed to reset password'],
    ['a network error', () => Promise.reject(new TypeError('Failed to fetch')),
      'Failed to fetch'],
  ])('keeps the dialog open with the error on %s', async (_name, fail, text) => {
    reply = fail
    await openFor('viewer@x.test')
    await answerConfirm()
    const error = await screen.findByTestId('confirm-dialog-error')
    expect(error).toHaveTextContent(text)
    const dialog = screen.getByRole('dialog')
    expect(dialog).toHaveAttribute('open')
    expect(dialog).toContainElement(error)
    expect(within(dialog).getByRole('heading')).toHaveTextContent('viewer@x.test')
    expect(rowFor('viewer@x.test')).toBeInTheDocument()
    expect(screen.queryByTestId('temp-password-notice')).toBeNull()
    expect(resetCalls()).toHaveLength(1)
  })

  it('Escape sends nothing and returns focus to the reset button', async () => {
    const user = userEvent.setup()
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('viewer@x.test')
    const button = within(rowFor('viewer@x.test')).getByTestId('user-reset-password-button')
    await user.click(button)
    expect(screen.getByRole('dialog')).toHaveAttribute('open')
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(document.activeElement).toBe(button)
    expect(resetCalls()).toHaveLength(0)
  })

  it('sends once when Confirm is clicked again while the request is in flight', async () => {
    let release
    reply = () => new Promise(res => { release = res })
    const dialog = await openFor('viewer@x.test')
    const confirmButton = within(dialog).getByTestId('confirm-dialog-confirm')
    fireEvent.click(confirmButton)
    fireEvent.click(confirmButton)
    await waitFor(() => expect(resetCalls()).toHaveLength(1))
    expect(confirmButton).toBeDisabled()
    expect(within(dialog).getByTestId('confirm-dialog-cancel')).toBeDisabled()
    release(ok({ temporary_password: temp }))
    await screen.findByTestId('temp-password-notice')
    expect(resetCalls()).toHaveLength(1)
  })

  it('opens one dialog at a time and names the next row after Cancel', async () => {
    await openFor('viewer@x.test')
    fireEvent.click(within(rowFor('ops@x.test')).getByTestId('user-reset-password-button'))
    expect(screen.getAllByRole('dialog')).toHaveLength(1)
    expect(await answerConfirm('cancel')).toBe('Reset password of viewer@x.test')
    fireEvent.click(within(rowFor('ops@x.test')).getByTestId('user-reset-password-button'))
    expect(await answerConfirm()).toBe('Reset password of ops@x.test')
    await screen.findByTestId('temp-password-notice')
    expect(resetCalls().map(([url]) => url)).toEqual(['/api/v1/users/7/password-reset'])
  })
})
