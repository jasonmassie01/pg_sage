import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { UsersPage } from './UsersPage'

// D7: admins see which users are bound to SSO, can unlink them, can issue a
// one-time link grant, and can create SSO-only users without a password.

const users = [
  { id: 1, email: 'admin@x.test', role: 'admin', created_at: '2026-09-01T00:00:00Z',
    last_login: null, sso_linked: false, sso_issuer: '', password_login: true },
  { id: 5, email: 'linked@x.test', role: 'operator', created_at: '2026-09-01T00:00:00Z',
    last_login: null, sso_linked: true, sso_issuer: 'https://idp.x.test',
    password_login: true },
  { id: 6, email: 'sso-only@x.test', role: 'viewer', created_at: '2026-09-01T00:00:00Z',
    last_login: null, sso_linked: false, sso_issuer: '', password_login: false },
]

function ok(body, status = 200) {
  return { ok: true, status, json: async () => body }
}

function rowFor(email) {
  return screen.getByText(email).closest('tr')
}

describe('UsersPage SSO controls', () => {
  beforeEach(() => {
    globalThis.fetch = vi.fn(url => {
      if (url === '/api/v1/users') return Promise.resolve(ok({ users }))
      return Promise.resolve(ok({ status: 'ok' }))
    })
    globalThis.confirm = vi.fn(() => true)
  })
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('shows an SSO badge and unlink for linked users', async () => {
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    const linked = await screen.findByText('linked@x.test')
    const row = linked.closest('tr')
    expect(within(row).getByTestId('user-sso-badge'))
      .toHaveAttribute('title', expect.stringContaining('https://idp.x.test'))
    expect(within(rowFor('admin@x.test')).queryByTestId('user-sso-badge')).toBeNull()

    fireEvent.click(within(row).getByTestId('user-sso-unlink'))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledWith(
      '/api/v1/users/5/oidc',
      expect.objectContaining({ method: 'DELETE', credentials: 'include' }),
    ))
    expect(globalThis.confirm).toHaveBeenCalled()
  })

  it('does not unlink when the admin cancels', async () => {
    globalThis.confirm = vi.fn(() => false)
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('linked@x.test')
    fireEvent.click(within(rowFor('linked@x.test')).getByTestId('user-sso-unlink'))
    const methods = globalThis.fetch.mock.calls.map(call => call[1]?.method)
    expect(methods).not.toContain('DELETE')
  })

  it('issues a one-time SSO link grant and shows the link once', async () => {
    globalThis.fetch.mockImplementation((url, opts) => {
      if (url === '/api/v1/users') return Promise.resolve(ok({ users }))
      if (url === '/api/v1/users/6/oidc-link-grant' && opts?.method === 'POST') {
        return Promise.resolve(ok({
          token: 'grant-token-abc', expires_at: '2026-09-27T12:15:00Z',
        }, 201))
      }
      return Promise.resolve(ok({}))
    })
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('sso-only@x.test')
    expect(within(rowFor('linked@x.test')).queryByTestId('user-sso-grant')).toBeNull()
    expect(within(rowFor('sso-only@x.test')).getByText('SSO only')).toBeInTheDocument()

    fireEvent.click(within(rowFor('sso-only@x.test')).getByTestId('user-sso-grant'))
    const notice = await screen.findByTestId('sso-grant-link')
    expect(notice).toHaveTextContent('#/link-sso?grant=grant-token-abc')
    expect(notice).toHaveTextContent('sso-only@x.test')
  })

  it('shows the server error when a grant cannot be issued', async () => {
    globalThis.fetch.mockImplementation(url => {
      if (url === '/api/v1/users') return Promise.resolve(ok({ users }))
      return Promise.resolve({
        ok: false, status: 409, json: async () => ({ error: 'user is already linked' }),
      })
    })
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('sso-only@x.test')
    fireEvent.click(within(rowFor('sso-only@x.test')).getByTestId('user-sso-grant'))
    expect(await screen.findByText('user is already linked')).toBeInTheDocument()
    expect(screen.queryByTestId('sso-grant-link')).toBeNull()
  })

  it('creates an SSO-only user without a password', async () => {
    render(<UsersPage currentUser={{ id: 1, role: 'admin' }} />)
    await screen.findByText('admin@x.test')
    fireEvent.change(screen.getByTestId('add-user-email'),
      { target: { value: 'new-sso@x.test' } })
    fireEvent.click(screen.getByTestId('add-user-sso-only'))
    expect(screen.getByTestId('add-user-password')).toBeDisabled()
    fireEvent.click(screen.getByTestId('add-user-submit'))

    await waitFor(() => {
      const post = globalThis.fetch.mock.calls.find(call =>
        call[0] === '/api/v1/users' && call[1]?.method === 'POST')
      expect(post).toBeTruthy()
      const body = JSON.parse(post[1].body)
      expect(body).toEqual({ email: 'new-sso@x.test', role: 'viewer', sso_only: true })
    })
  })
})
