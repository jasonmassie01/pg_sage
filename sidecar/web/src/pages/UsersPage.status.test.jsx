import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { UsersPage } from './UsersPage'
import { answerConfirm } from '../test/confirmDialog'

// apiauth-01: deactivation is the default offboarding step. Delete stays
// available, and both show the server's own error text.

const base = { created_at: '2026-09-01T00:00:00Z', last_login: null, sso_linked: false,
  sso_issuer: '', password_login: true }
const users = [
  { ...base, id: 1, email: 'admin@x.test', role: 'admin', disabled: false },
  { ...base, id: 2, email: 'old-admin@x.test', role: 'admin', disabled: true,
    disabled_at: '2026-10-01T00:00:00Z' },
  { ...base, id: 5, email: 'op@x.test', role: 'operator', disabled: false },
]

function ok(body, status = 200) {
  return { ok: true, status, json: async () => body }
}

function fail(status, error) {
  return { ok: false, status, json: async () => ({ error }) }
}

function rowFor(email) {
  return screen.getByText(email).closest('tr')
}

function calls(method) {
  return globalThis.fetch.mock.calls.filter(call => call[1]?.method === method)
}

describe('UsersPage deactivation', () => {
  beforeEach(() => {
    globalThis.fetch = vi.fn(url => {
      if (url === '/api/v1/users') return Promise.resolve(ok({ users }))
      return Promise.resolve(ok({ status: 'disabled', changed: true }))
    })
  })
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('deactivates a user after confirmation and reloads the list', async () => {
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('op@x.test')
    const button = within(rowFor('op@x.test')).getByTestId('user-status-button')
    expect(button).toHaveTextContent('Deactivate')
    fireEvent.click(button)
    expect(calls('PUT')).toHaveLength(0)
    expect(await answerConfirm()).toBe('Deactivate op@x.test')
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledWith(
      '/api/v1/users/5/status',
      expect.objectContaining({ method: 'PUT', body: '{"disabled":true}',
        credentials: 'include' }),
    ))
    await waitFor(() => expect(globalThis.fetch.mock.calls
      .filter(call => call[0] === '/api/v1/users')).toHaveLength(2))
  })

  it('does nothing when the admin cancels deactivation', async () => {
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('op@x.test')
    fireEvent.click(within(rowFor('op@x.test')).getByTestId('user-status-button'))
    await answerConfirm('cancel')
    expect(calls('PUT')).toHaveLength(0)
  })

  it('marks a deactivated user and offers reactivation', async () => {
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('old-admin@x.test')
    const row = rowFor('old-admin@x.test')
    expect(within(row).getByTestId('user-disabled-badge')).toHaveTextContent('Deactivated')
    expect(within(rowFor('op@x.test')).queryByTestId('user-disabled-badge')).toBeNull()
    const button = within(row).getByTestId('user-status-button')
    expect(button).toHaveTextContent('Reactivate')
    fireEvent.click(button)
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledWith(
      '/api/v1/users/2/status',
      expect.objectContaining({ method: 'PUT', body: '{"disabled":false}' }),
    ))
  })

  it('locks self and the last enabled admin, but not a deactivated admin', async () => {
    render(<UsersPage currentUser={{ id: 5, role: 'admin' }} />)
    await screen.findByText('admin@x.test')
    // admin@x.test is the only enabled admin: old-admin is deactivated.
    const last = rowFor('admin@x.test')
    expect(within(last).getByTestId('user-status-button')).toBeDisabled()
    expect(within(last).getByTestId('user-delete-button')).toBeDisabled()
    expect(within(last).getByTestId('user-role-select')).toBeDisabled()
    const self = rowFor('op@x.test')
    expect(within(self).getByTestId('user-status-button')).toBeDisabled()
    const old = rowFor('old-admin@x.test')
    expect(within(old).getByTestId('user-delete-button')).not.toBeDisabled()
    expect(within(old).getByTestId('user-status-button')).not.toBeDisabled()
  })

  it('shows the server error when deactivation is refused', async () => {
    globalThis.fetch = vi.fn((url, init) => {
      if (init?.method === 'PUT') {
        return Promise.resolve(fail(403, 'cannot deactivate the last enabled admin'))
      }
      return Promise.resolve(ok({ users }))
    })
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('op@x.test')
    fireEvent.click(within(rowFor('op@x.test')).getByTestId('user-status-button'))
    await answerConfirm()
    expect(await screen.findByText('cannot deactivate the last enabled admin'))
      .toBeInTheDocument()
  })

  it('shows the server error when delete is refused', async () => {
    const message = 'user is still referenced by sage.x; deactivate the user instead'
    globalThis.fetch = vi.fn((url, init) => {
      if (init?.method === 'DELETE') return Promise.resolve(fail(409, message))
      return Promise.resolve(ok({ users }))
    })
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('op@x.test')
    fireEvent.click(within(rowFor('op@x.test')).getByTestId('user-delete-button'))
    expect(await answerConfirm()).toBe('Delete user op@x.test')
    expect(await screen.findByText(message)).toBeInTheDocument()
    expect(screen.queryByText('Failed to delete user')).toBeNull()
  })
})
