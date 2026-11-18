import { act, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { AutonomyPage } from './AutonomyPage'
import { FleetOverview } from './fleet/FleetOverview'
import { DatabasePage } from './DatabasePage'
import { FactsPage } from './FactsPage'
import { SettingsPage } from './SettingsPage'
import { ShadowModePage } from './ShadowModePage'
import { TrustPage } from './TrustPage'
import { ValuePage } from './ValuePage'
import { DerivedSettings } from './settings/DerivedSettings'
import { useAPI } from '../hooks/useAPI'

// dashboard-01: every page that used to swap itself for an ErrorBanner on
// any error keeps its content under a failed refresh and shows the strip,
// and still shows the banner when it has no data at all. useAPI is mocked
// here only to put each page in a given state; the real hook is covered by
// CasesPage.stale.test.jsx and Actions.paging.test.jsx.
// No concurrent access tests: pure renders.

let state
const at = (h, m) => new Date(2026, 10, 8, h, m, 0).getTime()
const pageData = {
  '/api/v1/databases': { summary: { total_databases: 1, healthy: 1 }, databases: [] },
}
vi.mock('../hooks/useAPI', () => ({
  withTimeRange: base => base,
  useAPI: url => {
    if (!url) return { data: null, loading: false, error: null, refetch: vi.fn() }
    const path = url.split('?')[0]
    return { ...state, data: state.data && (pageData[path] || state.data) }
  },
}))

// App owns the fleet poll and hands the Fleet overview its query (dashboard-08).
function FleetOverviewWithFleet() {
  return <FleetOverview database="all" onSelectDB={() => {}}
    fleet={useAPI('/api/v1/databases')} />
}

const user = { role: 'admin' }
const pages = [
  ['AutonomyPage', <AutonomyPage database="prod" user={user} />, null],
  ['FleetOverview', <FleetOverviewWithFleet />, null],
  ['DatabasePage', <DatabasePage database="prod" />, /Database: prod/],
  ['FactsPage', <FactsPage database="prod" user={user} />, null],
  ['SettingsPage', <SettingsPage database="all" />, null],
  ['ShadowModePage', <ShadowModePage database="prod" />, /Shadow Mode/],
  ['TrustPage', <TrustPage database="prod" user={user} />, null],
  ['ValuePage', <ValuePage database="prod" />, /Value delivered/],
  ['DerivedSettings', <DerivedSettings database="prod" />, /Derived settings/],
]

beforeEach(() => {
  globalThis.fetch = vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) })
})

describe.each(pages)('%s', (_name, element, content) => {
  it('keeps its content and shows the strip when a refresh fails', async () => {
    state = { data: {}, loading: false, error: 'proxy down (502)', stale: true,
      updatedAt: at(14, 2), failedAt: at(14, 5), refetch: vi.fn() }
    await act(async () => { render(element) })
    const strips = screen.getAllByTestId('query-stale')
    expect(strips.length).toBeGreaterThan(0)
    // DerivedSettings words its errors "Derived settings: <message>".
    const failed = /^Refresh failed at 14:05 \((Derived settings: )?proxy down \(502\)\)\. /
    expect(strips[0]).toHaveTextContent(failed)
    expect(strips[0]).toHaveTextContent(/Showing data from 14:02/)
    if (content) expect(screen.getAllByText(content).length).toBeGreaterThan(0)
    expect(screen.queryByText(/^proxy down \(502\)$/, { selector: '[role=alert] *' }))
      .toBeNull()
  })

  it('shows the server message when the first load fails', async () => {
    state = { data: null, loading: false, error: 'proxy down (502)', stale: false,
      updatedAt: null, failedAt: at(14, 5), refetch: vi.fn() }
    await act(async () => { render(element) })
    expect(screen.queryAllByTestId('query-stale')).toHaveLength(0)
    expect(screen.getAllByText(/proxy down \(502\)/).length).toBeGreaterThan(0)
  })
})
