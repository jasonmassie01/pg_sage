import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Layout } from './Layout'

// security-08: the layout shows the secrets-at-rest banner to admins.

vi.mock('../hooks/useLiveEvents', () => ({ useLiveRefetch: () => {} }))
vi.mock('./TimeRangePicker', () => ({ TimeRangePicker: () => null }))

let posture
beforeEach(() => {
  globalThis.fetch = vi.fn(async url => ({
    ok: true, status: 200,
    json: async () => (url === '/api/v1/security/posture' ? posture : {}),
  }))
})
afterEach(() => { vi.restoreAllMocks() })

function renderLayout(role) {
  render(<Layout user={{ email: `${role}@example.com`, role }}
    databases={[]} selectedDB="all" onSelectDB={() => {}}
    fleetData={{ summary: {}, databases: [] }} onFleetChanged={() => {}}>
    <div />
  </Layout>)
}

describe('Layout secrets banner', () => {
  it('renders the banner for an admin when secrets are plaintext', async () => {
    posture = { secrets_at_rest: 'plaintext', key_source: 'none', plaintext_rows: 1 }
    renderLayout('admin')
    expect(await screen.findByTestId('secrets-posture-banner')).toBeInTheDocument()
  })

  it('does not ask for the posture for a viewer', async () => {
    posture = { secrets_at_rest: 'plaintext', key_source: 'none', plaintext_rows: 1 }
    renderLayout('viewer')
    await new Promise(resolve => setTimeout(resolve, 30))
    const urls = globalThis.fetch.mock.calls.map(([u]) => u)
    expect(urls).not.toContain('/api/v1/security/posture')
    expect(screen.queryByTestId('secrets-posture-banner')).toBeNull()
  })
})
