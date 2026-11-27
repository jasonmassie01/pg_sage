import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { DatabasesPage } from './DatabasesPage'

function mockFleet(readOnly) {
  const urls = []
  globalThis.fetch = vi.fn(async url => {
    urls.push(url)
    const bodies = {
      '/api/v1/config/global': { read_only: readOnly },
      '/api/v1/databases': { databases: [] },
      '/api/v1/databases/managed': { databases: [{ id: 1, name: 'main' }] },
      '/api/v1/fleet/findings': { findings: [], min_databases: 3, errors: [] },
      '/api/v1/databases/discovered': { discovered: [], sources: [] },
    }
    if (url in bodies) return { ok: true, status: 200, json: async () => bodies[url] }
    return { ok: false, status: 404, json: async () => ({}) }
  })
  return urls
}

describe('DatabasesPage Discovered tab (market-06)', () => {
  afterEach(() => { vi.restoreAllMocks() })

  it('switches to the Discovered tab and loads discovery only then', async () => {
    const urls = mockFleet(false)
    render(<DatabasesPage />)
    const tab = await screen.findByTestId('fleet-tab-discovered')
    expect(urls).not.toContain('/api/v1/databases/discovered')
    fireEvent.click(tab)
    expect(await screen.findByTestId('discovered-empty')).toBeInTheDocument()
    expect(urls).toContain('/api/v1/databases/discovered')
    expect(screen.getByTestId('fleet-tab-discovered')).toHaveAttribute('aria-selected', 'true')
  })

  // ux-11: every fleet now has the Health & readiness tab; a read-only
  // fleet still gets no Discovered tab.
  it('shows no Discovered tab in a read-only fleet', async () => {
    const urls = mockFleet(true)
    render(<DatabasesPage />)
    await waitFor(() => expect(screen.getByTestId('fleet-read-only')).toBeInTheDocument())
    expect(screen.queryByTestId('fleet-tab-discovered')).toBeNull()
    expect(screen.getByTestId('fleet-tab-readiness')).toBeInTheDocument()
    expect(urls).not.toContain('/api/v1/databases/discovered')
  })
})
