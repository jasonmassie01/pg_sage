import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { DatabasesPage } from './DatabasesPage'

// ux-11: Snapshot & metrics is gone. Its fleet health summary, provider
// readiness matrix and index admission panel are the Fleet page's Health
// & readiness tab; #/advanced opens it (tab=readiness). Every fleet has
// the tab, a read-only one too; only an editable fleet has Discovered.
// No concurrent access tests: one render tree.

vi.mock('./fleet/FleetOverview', () => ({
  FleetOverview: ({ database, fleet }) => (
    <div data-testid="fleet-overview-stub">{database}:{fleet?.data?.summary?.total_databases}</div>
  ),
}))

function mockFleet(readOnly) {
  const urls = []
  globalThis.fetch = vi.fn(async url => {
    urls.push(url)
    const bodies = {
      '/api/v1/config/global': { read_only: readOnly },
      '/api/v1/databases': { databases: [{ name: 'main' }] },
      '/api/v1/databases/managed': { databases: [{ id: 1, name: 'main' }] },
      '/api/v1/fleet/findings': { findings: [], min_databases: 3, errors: [] },
      '/api/v1/databases/discovered': { discovered: [], sources: [] },
    }
    if (url in bodies) return { ok: true, status: 200, json: async () => bodies[url] }
    return { ok: false, status: 404, json: async () => ({}) }
  })
  return urls
}

const fleet = { data: { summary: { total_databases: 3 }, databases: [] }, loading: false,
  error: null, refetch: vi.fn() }

describe('DatabasesPage Health & readiness tab', () => {
  afterEach(() => { vi.restoreAllMocks() })

  it('opens the databases tab by default and the readiness tab on a click', async () => {
    mockFleet(false)
    render(<DatabasesPage fleet={fleet} database="all" />)
    const tab = await screen.findByTestId('fleet-tab-readiness')
    expect(tab).toHaveAttribute('aria-selected', 'false')
    expect(screen.queryByTestId('fleet-overview-stub')).toBeNull()
    fireEvent.click(tab)
    expect(screen.getByTestId('fleet-overview-stub')).toHaveTextContent('all:3')
    expect(tab).toHaveAttribute('aria-selected', 'true')
  })

  it('opens on the readiness tab when the route asks for it', async () => {
    mockFleet(false)
    render(<DatabasesPage fleet={fleet} database="main" initialTab="readiness" />)
    expect(await screen.findByTestId('fleet-overview-stub')).toHaveTextContent('main:3')
    expect(screen.getByTestId('fleet-tab-readiness')).toHaveAttribute('aria-selected', 'true')
  })

  it('gives a read-only fleet the readiness tab but not Discovered', async () => {
    mockFleet(true)
    render(<DatabasesPage fleet={fleet} database="all" />)
    await waitFor(() => expect(screen.getByTestId('fleet-read-only')).toBeInTheDocument())
    expect(screen.queryByTestId('fleet-tab-discovered')).toBeNull()
    fireEvent.click(screen.getByTestId('fleet-tab-readiness'))
    expect(screen.getByTestId('fleet-overview-stub')).toBeInTheDocument()
  })

  it('ignores an unknown tab from the route', async () => {
    mockFleet(true)
    render(<DatabasesPage fleet={fleet} database="all" initialTab="discovered" />)
    await waitFor(() => expect(screen.getByTestId('fleet-read-only')).toBeInTheDocument())
    expect(screen.getByTestId('fleet-tab-databases')).toHaveAttribute('aria-selected', 'true')
    expect(screen.queryByTestId('fleet-overview-stub')).toBeNull()
  })
})
