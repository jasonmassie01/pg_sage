import { act, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { LoginPage } from './LoginPage'

// apiauth-07: while the identity provider cannot be reached the SSO button
// stays visible but disabled, and it comes back without a reload.

function configResponse(body) {
  return Promise.resolve({ ok: true, json: async () => body })
}

describe('LoginPage SSO readiness', () => {
  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('disables the button while SSO is not ready', async () => {
    globalThis.fetch = vi.fn(() => configResponse({
      enabled: true, ready: false, provider: 'oidc',
      reason: 'identity provider unreachable',
    }))
    render(<LoginPage onLogin={vi.fn()} />)
    const button = await screen.findByTestId('oauth-login')
    expect(button).toBeDisabled()
    expect(button).toHaveTextContent('SSO temporarily unavailable')
  })

  it('keeps the button enabled for a server without the ready field', async () => {
    globalThis.fetch = vi.fn(() => configResponse({ enabled: true, provider: 'oidc' }))
    render(<LoginPage onLogin={vi.fn()} />)
    const button = await screen.findByTestId('oauth-login')
    expect(button).not.toBeDisabled()
    expect(button).toHaveTextContent('Sign in with SSO')
  })

  it('enables the button once a later config check reports ready', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const bodies = [
      { enabled: true, ready: false, provider: 'oidc', reason: 'misconfigured' },
      { enabled: true, ready: true, provider: 'oidc' },
    ]
    globalThis.fetch = vi.fn(() => configResponse(
      bodies.length > 1 ? bodies.shift() : bodies[0]))
    render(<LoginPage onLogin={vi.fn()} />)
    expect(await screen.findByTestId('oauth-login')).toBeDisabled()
    await act(async () => { await vi.advanceTimersByTimeAsync(31000) })
    const button = await screen.findByTestId('oauth-login')
    expect(button).not.toBeDisabled()
    expect(button).toHaveTextContent('Sign in with SSO')
  })

  it('shows no SSO button when SSO is disabled', async () => {
    globalThis.fetch = vi.fn(() => configResponse({ enabled: false, provider: '' }))
    render(<LoginPage onLogin={vi.fn()} />)
    await act(async () => { await Promise.resolve() })
    expect(screen.queryByTestId('oauth-login')).toBeNull()
  })
})
