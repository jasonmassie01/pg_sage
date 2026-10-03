import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AutonomyPage } from './AutonomyPage'

// Roadmap 1.1 (2026-10-03): the promotion coach shows where each family's
// bench evidence comes from (a signed release report, a local run on a
// clone, or an unsigned operator upload) and offers "Run bench locally"
// on every pair whose next level is missing bench evidence. Only an admin
// starts a run; it needs a clone provider or a disposable local database.

const refetch = vi.fn()

const benchUnmet = [
  { name: 'bench_present', met: false, observed: 'no PGIncidentBench report',
    required: 'a gated arm', how: 'Run the bench locally on a clone, or upload a report' },
  { name: 'shadow_volume', met: false, observed: '1', required: '>= 3',
    how: 'Review 2 more investigations' },
]

const view = {
  database: 'orders', enforced: true, databases: ['orders'],
  view: {
    generated_at: '2026-10-03T12:00:00Z', game_days: [],
    families: [
      {
        family: 'lock_blocking', shadow: { reviewed: 1, accepted: 1 },
        bench: {
          id: 'r1', origin: 'signed_release', signed: true,
          provenance: 'signed release report (pg_sage 1.8.5, commit ccccccc)',
          generated_at: '2026-10-03T10:00:00Z', families: ['lock_blocking'],
        },
        classes: [{
          class: 'backend_cancel', granted: 'L1', cap: 'L2',
          next: { target: 'L2', met: false, checks: benchUnmet },
        }],
      },
      {
        family: 'wal_retention', shadow: { reviewed: 0, accepted: 0 },
        bench: {
          id: 'r2', origin: 'operator', signed: false,
          provenance: 'unsigned (operator-provided)',
          generated_at: '2026-10-01T10:00:00Z', families: ['wal_retention'],
        },
        classes: [{
          class: 'slot_drop', granted: 'L1', cap: 'L2',
          next: { target: 'L2', met: false, checks: [benchUnmet[1]] },
        }],
      },
      {
        family: 'sequence_runway', shadow: { reviewed: 0, accepted: 0 },
        classes: [{
          class: 'sequence_bump', granted: 'L1', cap: 'L2',
          next: { target: 'L2', met: false, checks: [benchUnmet[0]] },
        }],
      },
    ],
  },
}

let benchState

vi.mock('../hooks/useAPI', () => ({
  useAPI: path => {
    if (path?.startsWith('/api/v1/sre/autonomy/bench-runs')) {
      return { ...benchState, loading: false, error: null, refetch }
    }
    if (path?.startsWith('/api/v1/sre/autonomy/history')) {
      return { data: { items: [] }, loading: false, error: null, refetch }
    }
    if (path?.startsWith('/api/v1/sre/autonomy/game-days')) {
      return { data: { enabled: false, items: [] }, loading: false, error: null, refetch }
    }
    if (path?.startsWith('/api/v1/sre/autonomy/rollouts')) {
      return { data: { items: [] }, loading: false, error: null, refetch }
    }
    return { data: view, loading: false, error: null, refetch }
  },
}))

const admin = { role: 'admin' }
const operator = { role: 'operator' }

function panel() {
  return screen.getByTestId('path-to-next-level')
}

describe('Path to next level: bench evidence', () => {
  beforeEach(() => {
    benchState = { data: { enabled: true, provider: 'dle', running: false, last: null } }
    refetch.mockClear()
  })
  afterEach(() => vi.unstubAllGlobals())

  it('shows the provenance of every family bench report', () => {
    render(<AutonomyPage database="orders" user={operator} />)
    const lock = within(panel()).getByTestId('bench-provenance-lock_blocking')
    expect(lock).toHaveTextContent('signed release report (pg_sage 1.8.5, commit ccccccc)')
    const wal = within(panel()).getByTestId('bench-provenance-wal_retention')
    expect(wal).toHaveTextContent('unsigned (operator-provided)')
    const seq = within(panel()).getByTestId('bench-provenance-sequence_runway')
    expect(seq).toHaveTextContent(/no bench report/i)
  })

  it('offers Run bench locally only where bench evidence is missing', () => {
    render(<AutonomyPage database="orders" user={admin} />)
    const lockPair = within(panel()).getByTestId('path-lock_blocking-backend_cancel')
    expect(within(lockPair).getByRole('button', { name: /run bench locally/i }))
      .toBeEnabled()
    const walPair = within(panel()).getByTestId('path-wal_retention-slot_drop')
    expect(within(walPair).queryByRole('button', { name: /run bench locally/i }))
      .toBeNull()
  })

  it('starts a local run of the family for the selected database', async () => {
    const fetch = vi.fn(() => Promise.resolve({ ok: true, status: 202,
      json: () => Promise.resolve({ run: { id: 'b1', status: 'running' } }) }))
    vi.stubGlobal('fetch', fetch)
    render(<AutonomyPage database="orders" user={admin} />)
    const pair = within(panel()).getByTestId('path-sequence_runway-sequence_bump')
    fireEvent.click(within(pair).getByRole('button', { name: /run bench locally/i }))
    await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1))
    const [url, opts] = fetch.mock.calls[0]
    expect(url).toBe('/api/v1/sre/autonomy/bench-runs?database=orders')
    expect(opts.method).toBe('POST')
    expect(JSON.parse(opts.body)).toEqual({ families: ['sequence_runway'] })
    await waitFor(() => expect(refetch).toHaveBeenCalled())
  })

  it('explains how to enable local runs when no disposable target exists', () => {
    benchState = { data: { enabled: false,
      how: 'Set clone.provider (dle or snapshot) or sre.autonomy.game_days.local_dsn' } }
    render(<AutonomyPage database="orders" user={admin} />)
    const pair = within(panel()).getByTestId('path-lock_blocking-backend_cancel')
    expect(within(pair).getByRole('button', { name: /run bench locally/i })).toBeDisabled()
    expect(within(panel()).getByTestId('local-bench-status'))
      .toHaveTextContent('clone.provider')
  })

  it('lets only an admin start a run', () => {
    render(<AutonomyPage database="orders" user={operator} />)
    const pair = within(panel()).getByTestId('path-lock_blocking-backend_cancel')
    expect(within(pair).queryByRole('button', { name: /run bench locally/i })).toBeNull()
    expect(pair).toHaveTextContent(/an admin can run the bench locally/i)
  })

  it('shows a running local run and disables another', () => {
    benchState = { data: { enabled: true, provider: 'local', running: true,
      last: { id: 'b1', status: 'running', families: ['lock_blocking'],
        started_at: '2026-10-03T12:00:00Z' } } }
    render(<AutonomyPage database="orders" user={admin} />)
    expect(within(panel()).getByTestId('local-bench-status'))
      .toHaveTextContent(/running/i)
    const pair = within(panel()).getByTestId('path-lock_blocking-backend_cancel')
    expect(within(pair).getByRole('button', { name: /run bench locally/i })).toBeDisabled()
  })

  it('shows why the last local run failed', () => {
    benchState = { data: { enabled: true, provider: 'local', running: false,
      last: { id: 'b1', status: 'failed', families: ['lock_blocking'],
        error: 'ingest: report is for another pg_sage build' } } }
    render(<AutonomyPage database="orders" user={admin} />)
    expect(within(panel()).getByTestId('local-bench-status'))
      .toHaveTextContent('report is for another pg_sage build')
  })

  it('shows a refused start', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve({ ok: false, status: 409,
      json: () => Promise.resolve({ error: 'a local bench run is already running' }) })))
    render(<AutonomyPage database="orders" user={admin} />)
    const pair = within(panel()).getByTestId('path-lock_blocking-backend_cancel')
    fireEvent.click(within(pair).getByRole('button', { name: /run bench locally/i }))
    expect(await screen.findByRole('alert'))
      .toHaveTextContent('a local bench run is already running')
  })
})
