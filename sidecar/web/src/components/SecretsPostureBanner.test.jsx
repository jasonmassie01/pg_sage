import { act, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { SecretsPostureBanner, SETTINGS_SAVED_EVENT } from './SecretsPostureBanner'

// security-08: admins see a persistent banner whenever stored secrets are
// not sealed at rest.

let fetchMock
function respond(body, ok = true) {
  fetchMock.mockResolvedValue({ ok, status: ok ? 200 : 500, json: async () => body })
}
beforeEach(() => {
  fetchMock = vi.fn()
  globalThis.fetch = fetchMock
})
afterEach(() => { vi.restoreAllMocks() })

const postureCalls = () => fetchMock.mock.calls
  .filter(([url]) => url === '/api/v1/security/posture').length

describe('SecretsPostureBanner', () => {
  it('warns about plaintext secrets with the row count and the fix', async () => {
    respond({ secrets_at_rest: 'plaintext', key_source: 'none', plaintext_rows: 2 })
    render(<SecretsPostureBanner isAdmin />)
    const banner = await screen.findByTestId('secrets-posture-banner')
    expect(banner).toHaveTextContent(/plaintext/i)
    expect(banner).toHaveTextContent('2')
    expect(banner).toHaveTextContent(/data_dir|encryption_key/)
    expect(banner).toHaveAttribute('role', 'alert')
  })

  it('names a lost generated key and the restore step', async () => {
    respond({ secrets_at_rest: 'key_missing', key_source: 'none', plaintext_rows: 0 })
    render(<SecretsPostureBanner isAdmin />)
    const banner = await screen.findByTestId('secrets-posture-banner')
    expect(banner).toHaveTextContent(/encryption\.key/)
    expect(banner).toHaveTextContent(/restore/i)
  })

  it('stays hidden when secrets are sealed', async () => {
    respond({ secrets_at_rest: 'sealed', key_source: 'generated', plaintext_rows: 0 })
    render(<SecretsPostureBanner isAdmin />)
    await waitFor(() => expect(postureCalls()).toBe(1))
    expect(screen.queryByTestId('secrets-posture-banner')).toBeNull()
  })

  it('never fetches for non-admins', () => {
    respond({ secrets_at_rest: 'plaintext', key_source: 'none', plaintext_rows: 1 })
    render(<SecretsPostureBanner isAdmin={false} />)
    expect(postureCalls()).toBe(0)
    expect(screen.queryByTestId('secrets-posture-banner')).toBeNull()
  })

  it('stays hidden when the posture cannot be read', async () => {
    respond({ error: 'security posture is unavailable' }, false)
    render(<SecretsPostureBanner isAdmin />)
    await waitFor(() => expect(postureCalls()).toBe(1))
    expect(screen.queryByTestId('secrets-posture-banner')).toBeNull()
  })

  it('fetches once, then again after a Settings save', async () => {
    respond({ secrets_at_rest: 'plaintext', key_source: 'none', plaintext_rows: 1 })
    render(<SecretsPostureBanner isAdmin />)
    await screen.findByTestId('secrets-posture-banner')
    expect(postureCalls()).toBe(1)
    respond({ secrets_at_rest: 'sealed', key_source: 'configured', plaintext_rows: 0 })
    act(() => { window.dispatchEvent(new CustomEvent(SETTINGS_SAVED_EVENT)) })
    await waitFor(() =>
      expect(screen.queryByTestId('secrets-posture-banner')).toBeNull())
    expect(postureCalls()).toBe(2)
  })
})
