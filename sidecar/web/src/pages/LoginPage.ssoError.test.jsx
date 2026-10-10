import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LoginPage } from './LoginPage'

// D7 follow-up: a failed SSO callback lands on the login page with an
// sso_error code, which renders as a readable message, not raw JSON.

describe('LoginPage SSO callback errors', () => {
  beforeEach(() => {
    globalThis.fetch = vi.fn(() => Promise.resolve({
      ok: true, json: async () => ({ enabled: true, provider: 'oidc' }),
    }))
  })
  afterEach(() => {
    vi.restoreAllMocks()
    window.history.replaceState(null, '', '/')
  })

  function landOn(hash) {
    window.history.replaceState(null, '', '/' + hash)
    render(<LoginPage onLogin={vi.fn()} />)
  }

  it('explains a 403 link-required refusal', async () => {
    landOn('#/login?sso_error=link_required')
    const alert = await screen.findByTestId('sso-error')
    expect(alert).toHaveTextContent(/already exists/)
    expect(alert).toHaveTextContent(/link SSO from your account page/)
    expect(alert).not.toHaveTextContent('{')
  })

  it('explains a 409 link conflict', async () => {
    landOn('#/login?sso_error=link_conflict')
    expect(await screen.findByTestId('sso-error'))
      .toHaveTextContent(/cannot be linked/)
  })

  it('explains a 401 unverified email and a failed sign-in', async () => {
    landOn('#/login?sso_error=unverified')
    expect(await screen.findByTestId('sso-error'))
      .toHaveTextContent(/did not confirm your email/)
  })

  it('explains a 403 refusal for a user no group maps to a role', async () => {
    landOn('#/login?sso_error=not_authorized')
    const alert = await screen.findByTestId('sso-error')
    expect(alert).toHaveTextContent(/not in a group that is allowed/)
    expect(alert).toHaveTextContent(/administrator/)
  })

  it('shows a generic message for a failed or unknown code', async () => {
    landOn('#/login?sso_error=<b>injected</b>')
    const alert = await screen.findByTestId('sso-error')
    expect(alert).toHaveTextContent(/Single sign-on failed/)
    expect(alert).not.toHaveTextContent('injected')
  })

  it('removes the error code from the address after showing it', async () => {
    landOn('#/login?sso_error=failed')
    await screen.findByTestId('sso-error')
    expect(window.location.hash).toBe('#/login')
  })

  it('shows nothing without an error code', async () => {
    landOn('#/')
    await screen.findByTestId('oauth-login')
    expect(screen.queryByTestId('sso-error')).toBeNull()
  })
})
