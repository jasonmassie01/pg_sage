import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AutonomyPage } from './AutonomyPage'

// Phase 1.1 promotion coach: "Evaluate now" asks pg_sage to propose what
// the evidence supports and shows, for every pair it did not propose, why;
// "Path to next level" turns each unmet check into an instruction with
// its counts and, where the rule implies one, an ETA. Earned autonomy is
// per database, so "All databases" asks the user to pick one.

const refetch = vi.fn()

const lockRow = {
  class: 'backend_cancel', reversibility: 'mitigation_only', cap: 'L2',
  granted: 'L1', supported: 'L1', effective: 'L1', version: 0,
  next: {
    target: 'L2', met: false,
    checks: [
      { name: 'shadow_volume', met: false, observed: '1', required: '>= 3 reviewed packets',
        how: 'Review 2 more concluded or inconclusive investigations in the last 4 h' },
      { name: 'shadow_duration', met: false, observed: '1h0m0s', required: '>= 4h0m0s',
        how: 'Keep reviewing: the 4 h window has 3 h to go',
        eta: '2026-10-02T15:00:00Z' },
      { name: 'bench_present', met: false, observed: 'no PGIncidentBench report',
        required: 'a gated arm' },
      { name: 'family_ships', met: true, observed: 'lock_blocking', required: 'shipped' },
    ],
  },
  live: { verified_l2_recoveries: 0, harmful: 0 },
}

const view = {
  database: 'orders', enforced: true, databases: ['orders', 'billing'],
  view: {
    generated_at: '2026-10-02T12:00:00Z', game_days: [],
    families: [
      { family: 'lock_blocking', shadow: { reviewed: 1, accepted: 1 }, classes: [lockRow] },
      {
        family: 'wal_retention', shadow: { reviewed: 0, accepted: 0 },
        classes: [{
          class: 'slot_drop', reversibility: 'irreversible', cap: 'L1', granted: 'L1',
          supported: 'L1', effective: 'L1', version: 0,
          live: { verified_l2_recoveries: 0, harmful: 0 },
        }],
      },
    ],
  },
}

let viewState = { data: view, error: null }

vi.mock('../hooks/useAPI', () => ({
  useAPI: path => {
    if (path?.startsWith('/api/v1/sre/autonomy/history')) {
      return { data: { items: [] }, loading: false, error: null, refetch }
    }
    if (path?.startsWith('/api/v1/sre/autonomy/game-days')) {
      return { data: { enabled: false, items: [] }, loading: false, error: null, refetch }
    }
    if (path?.startsWith('/api/v1/sre/autonomy/rollouts')) {
      return { data: { items: [] }, loading: false, error: null, refetch }
    }
    return { ...viewState, loading: false, refetch }
  },
}))

const operator = { role: 'operator' }
const viewer = { role: 'viewer' }

describe('AutonomyPage promotion coach', () => {
  beforeEach(() => {
    viewState = { data: view, error: null }
    refetch.mockClear()
  })
  afterEach(() => vi.unstubAllGlobals())

  it('lists every unmet check as an instruction with its ETA', () => {
    render(<AutonomyPage database="orders" user={viewer} />)
    const panel = screen.getByTestId('path-to-next-level')
    const pair = within(panel).getByTestId('path-lock_blocking-backend_cancel')
    expect(pair).toHaveTextContent('L1 to L2')
    expect(pair).toHaveTextContent('Review 2 more concluded or inconclusive investigations')
    expect(pair).toHaveTextContent('3 h to go')
    expect(within(pair).getByTestId('eta-shadow_duration')).toHaveTextContent('ETA')
    // A check without an instruction still says what is missing.
    expect(pair).toHaveTextContent('bench_present: no PGIncidentBench report')
    // Met checks are not instructions.
    expect(pair).not.toHaveTextContent('family_ships')
    // A pair at its cap has no path.
    expect(within(panel).queryByTestId('path-wal_retention-slot_drop')).toBeNull()
  })

  it('says when every pair is at its cap or proposed', () => {
    viewState = { data: { ...view, view: { ...view.view, families: [view.view.families[1]] } },
      error: null }
    render(<AutonomyPage database="orders" user={viewer} />)
    expect(screen.getByTestId('path-to-next-level'))
      .toHaveTextContent(/nothing left to earn|at its cap/i)
  })

  it('offers Evaluate now to operators only', () => {
    const { unmount } = render(<AutonomyPage database="orders" user={viewer} />)
    expect(screen.queryByRole('button', { name: /evaluate now/i })).toBeNull()
    unmount()
    render(<AutonomyPage database="orders" user={operator} />)
    expect(screen.getByRole('button', { name: /evaluate now/i })).toBeInTheDocument()
  })

  it('evaluates the selected database and shows what was not proposed and why', async () => {
    const fetch = vi.fn(() => Promise.resolve({ ok: true, status: 200,
      json: () => Promise.resolve({
        created: [],
        not_proposed: [
          { family: 'lock_blocking', class: 'backend_cancel', granted: 'L1', target: 'L2',
            reason: 'evidence_not_met', unmet: [lockRow.next.checks[0]] },
          { family: 'wal_retention', class: 'slot_drop', granted: 'L1',
            reason: 'at_cap' },
        ],
      }) }))
    vi.stubGlobal('fetch', fetch)
    render(<AutonomyPage database="orders" user={operator} />)
    fireEvent.click(screen.getByRole('button', { name: /evaluate now/i }))
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1))
    const [url, opts] = fetch.mock.calls[0]
    expect(url).toBe('/api/v1/sre/autonomy/evaluate?database=orders')
    expect(opts.method).toBe('POST')
    const result = await screen.findByTestId('evaluate-result')
    expect(result).toHaveTextContent(/no new promotion/i)
    expect(result).toHaveTextContent('lock_blocking / backend_cancel')
    expect(result).toHaveTextContent('Review 2 more concluded')
    expect(result).toHaveTextContent(/at its cap/i)
    await waitFor(() => expect(refetch).toHaveBeenCalled())
  })

  it('reports created proposals', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve({ ok: true, status: 200,
      json: () => Promise.resolve({
        created: [{ id: 'p1', family: 'lock_blocking', class: 'backend_cancel',
          from: 'L1', to: 'L2' }],
        not_proposed: [],
      }) })))
    render(<AutonomyPage database="orders" user={operator} />)
    fireEvent.click(screen.getByRole('button', { name: /evaluate now/i }))
    const result = await screen.findByTestId('evaluate-result')
    expect(result).toHaveTextContent(/proposed 1/i)
    expect(result).toHaveTextContent('lock_blocking / backend_cancel: L1 to L2')
    expect(result).toHaveTextContent(/admin/i)
  })

  it('shows an evaluation failure', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve({ ok: false, status: 503,
      json: () => Promise.resolve({ error: 'autonomy ledger unavailable' }) })))
    render(<AutonomyPage database="orders" user={operator} />)
    fireEvent.click(screen.getByRole('button', { name: /evaluate now/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent('autonomy ledger unavailable')
    expect(screen.queryByTestId('evaluate-result')).toBeNull()
  })

  it('asks for one database when All databases is selected in a fleet', () => {
    viewState = { data: null, error: '400 Bad Request' }
    render(<AutonomyPage database="all" user={viewer} />)
    expect(screen.getByTestId('autonomy-pick-database'))
      .toHaveTextContent(/per database/i)
  })
})
