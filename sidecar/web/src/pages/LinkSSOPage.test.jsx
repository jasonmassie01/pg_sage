import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { LinkSSOPage } from './LinkSSOPage'

// D7: a user who cannot sign in with a password redeems an admin-issued
// one-time grant, then completes the SSO round trip.

function ok(body) {
  return { ok: true, status: 200, json: async () => body }
}

describe('LinkSSOPage', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    window.location.hash = ''
  })

  it('redeems the grant from the link and redirects to the provider', async () => {
    window.location.hash = '#/link-sso?grant=grant-xyz'
    globalThis.fetch = vi.fn(() => Promise.resolve(ok({
      url: 'https://idp.x.test/auth?state=g1',
    })))
    const navigate = vi.fn()
    render(<LinkSSOPage navigate={navigate} />)
    fireEvent.click(screen.getByTestId('link-sso-grant-continue'))
    await waitFor(() => expect(navigate)
      .toHaveBeenCalledWith('https://idp.x.test/auth?state=g1'))
    const [url, opts] = globalThis.fetch.mock.calls[0]
    expect(url).toBe('/api/v1/auth/oauth/link-grant')
    expect(opts.method).toBe('POST')
    expect(JSON.parse(opts.body)).toEqual({ grant: 'grant-xyz' })
  })

  it('shows the error for an invalid or used grant', async () => {
    window.location.hash = '#/link-sso?grant=used'
    globalThis.fetch = vi.fn(() => Promise.resolve({
      ok: false, status: 400,
      json: async () => ({ error: 'invalid or expired link grant' }),
    }))
    const navigate = vi.fn()
    render(<LinkSSOPage navigate={navigate} />)
    fireEvent.click(screen.getByTestId('link-sso-grant-continue'))
    expect(await screen.findByText('invalid or expired link grant')).toBeInTheDocument()
    expect(navigate).not.toHaveBeenCalled()
  })

  it('removes the grant from the address and history before any request', () => {
    window.location.hash = '#/link-sso?grant=secret-grant-value'
    const replace = vi.spyOn(window.history, 'replaceState')
    globalThis.fetch = vi.fn()
    render(<LinkSSOPage navigate={vi.fn()} />)
    expect(window.location.hash).toBe('#/link-sso')
    expect(window.location.href).not.toContain('secret-grant-value')
    expect(replace).toHaveBeenCalled()
    const [, , url] = replace.mock.calls[replace.mock.calls.length - 1]
    expect(String(url)).not.toContain('secret-grant-value')
    expect(globalThis.fetch).not.toHaveBeenCalled()
    expect(screen.getByTestId('link-sso-grant-continue')).toBeEnabled()
  })

  it('still redeems the grant it read after scrubbing the address', async () => {
    window.location.hash = '#/link-sso?grant=kept-in-memory'
    globalThis.fetch = vi.fn(() => Promise.resolve(ok({ url: 'https://idp.x.test/a' })))
    render(<LinkSSOPage navigate={vi.fn()} />)
    expect(window.location.hash).toBe('#/link-sso')
    fireEvent.click(screen.getByTestId('link-sso-grant-continue'))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledTimes(1))
    expect(JSON.parse(globalThis.fetch.mock.calls[0][1].body))
      .toEqual({ grant: 'kept-in-memory' })
  })

  it('disables the button when the link has no grant', () => {
    window.location.hash = '#/link-sso'
    globalThis.fetch = vi.fn()
    render(<LinkSSOPage navigate={vi.fn()} />)
    expect(screen.getByTestId('link-sso-grant-continue')).toBeDisabled()
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })
})
