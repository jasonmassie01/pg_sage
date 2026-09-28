import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ProfilePage } from './ProfilePage'

// D7: a signed-in user links SSO to their own account from the profile page.

const user = { id: 3, email: 'me@x.test', role: 'operator' }

function ok(body) {
  return { ok: true, status: 200, json: async () => body }
}

describe('ProfilePage SSO linking', () => {
  beforeEach(() => {
    window.location.hash = '#/profile'
  })
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('Link SSO button calls the authorize endpoint with intent=link', async () => {
    globalThis.fetch = vi.fn(url => {
      if (url === '/api/v1/auth/sso') {
        return Promise.resolve(ok({ linked: false, oauth_enabled: true, provider: 'oidc' }))
      }
      return Promise.resolve(ok({ url: 'https://idp.x.test/auth?state=s1' }))
    })
    const navigate = vi.fn()
    render(<ProfilePage user={user} navigate={navigate} />)
    expect(await screen.findByText('me@x.test')).toBeInTheDocument()
    fireEvent.click(await screen.findByTestId('link-sso-button'))
    await waitFor(() => expect(navigate)
      .toHaveBeenCalledWith('https://idp.x.test/auth?state=s1'))
    expect(globalThis.fetch).toHaveBeenCalledWith(
      '/api/v1/auth/oauth/authorize?intent=link',
      expect.objectContaining({ credentials: 'include' }),
    )
  })

  it('shows the linked identity instead of the link button', async () => {
    globalThis.fetch = vi.fn(() => Promise.resolve(ok({
      linked: true, issuer: 'https://idp.x.test', oauth_enabled: true,
    })))
    window.location.hash = '#/profile?linked=1'
    render(<ProfilePage user={user} navigate={vi.fn()} />)
    expect(await screen.findByTestId('sso-linked-status'))
      .toHaveTextContent('https://idp.x.test')
    expect(screen.getByTestId('sso-link-success')).toBeInTheDocument()
    expect(screen.queryByTestId('link-sso-button')).toBeNull()
  })

  it('explains when SSO is not configured', async () => {
    globalThis.fetch = vi.fn(() => Promise.resolve(ok({
      linked: false, oauth_enabled: false,
    })))
    render(<ProfilePage user={user} navigate={vi.fn()} />)
    expect(await screen.findByText(/SSO is not configured/)).toBeInTheDocument()
    expect(screen.queryByTestId('link-sso-button')).toBeNull()
  })

  it('shows the error when the authorize request fails', async () => {
    globalThis.fetch = vi.fn(url => {
      if (url === '/api/v1/auth/sso') {
        return Promise.resolve(ok({ linked: false, oauth_enabled: true }))
      }
      return Promise.resolve({
        ok: false, status: 401, json: async () => ({ error: 'authentication required' }),
      })
    })
    const navigate = vi.fn()
    render(<ProfilePage user={user} navigate={navigate} />)
    fireEvent.click(await screen.findByTestId('link-sso-button'))
    expect(await screen.findByText('authentication required')).toBeInTheDocument()
    expect(navigate).not.toHaveBeenCalled()
  })
})
