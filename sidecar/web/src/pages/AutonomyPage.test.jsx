import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AutonomyPage } from './AutonomyPage'

// Sage SRE M7: the earned-autonomy view per family x action class with
// level, evidence and history; an admin approves a pg_sage promotion
// proposal; operators reject and downgrade; viewers only read.

const refetch = vi.fn()

const view = {
  database: 'orders',
  enforced: true,
  databases: ['orders'],
  view: {
    generated_at: '2026-10-02T12:00:00Z',
    bench: { id: 'bench-1', generated_at: '2026-10-01T10:00:00Z', source: 'bench' },
    game_days: [],
    families: [
      {
        family: 'wal_retention',
        shadow: { reviewed: 24, accepted: 24, first_review_at: '2026-08-30T00:00:00Z' },
        classes: [
          {
            class: 'wal_bound', reversibility: 'mitigation_only', cap: 'L2',
            granted: 'L1', supported: 'L2', effective: 'L1', version: 0,
            downgrades: [{ reason: 'ha_role_not_primary', detail: 'role replica' }],
            pending: {
              id: '11111111-1111-4111-8111-111111111111', family: 'wal_retention',
              class: 'wal_bound', from: 'L1', to: 'L2', status: 'pending',
              proposed_at: '2026-10-02T11:00:00Z', expires_at: '2026-10-09T11:00:00Z',
            },
            next: {
              target: 'L2', met: true,
              checks: [{ name: 'bench_top1', met: true, observed: '11/12', required: '>= 80%' }],
            },
            live: { verified_l2_recoveries: 0, harmful: 0 },
          },
          {
            class: 'slot_drop', reversibility: 'irreversible', cap: 'L1',
            granted: 'L1', supported: 'L1', effective: 'L1', version: 0,
            live: { verified_l2_recoveries: 0, harmful: 0 },
          },
        ],
      },
      {
        family: 'wraparound_runway',
        shadow: { reviewed: 0, accepted: 0 },
        classes: [
          {
            class: 'freeze', reversibility: 'reversible', cap: 'L3',
            granted: 'L3', supported: 'L1', effective: 'L3', version: 1,
            provenance: 'carried_over',
            carried_ref: 'spec F3: the wraparound freeze custodian ran autonomously',
            live: { verified_l2_recoveries: 0, harmful: 0 },
          },
        ],
      },
      {
        family: 'lock_blocking',
        shadow: { reviewed: 3, accepted: 2 },
        classes: [
          {
            class: 'backend_cancel', reversibility: 'mitigation_only', cap: 'L2',
            granted: 'L1', supported: 'L1', effective: 'L1', version: 0,
            next: {
              target: 'L2', met: false,
              checks: [
                {
                  name: 'shadow_volume', met: false, observed: '3',
                  required: '>= 20 reviewed packets in the window',
                },
                {
                  name: 'bench_present', met: true, observed: '1 gated cells',
                  required: 'a gated arm',
                },
              ],
            },
            live: { verified_l2_recoveries: 0, harmful: 0 },
          },
        ],
      },
    ],
  },
}

const history = {
  items: [{
    id: 5, family: 'wal_retention', class: 'wal_bound', type: 'promotion_proposed',
    from: 'L1', to: 'L2', actor: 'pg_sage', reason: 'evidence supports L2',
    at: '2026-10-02T11:00:00Z',
  }],
}

const gameDaysOff = { enabled: false, items: [] }

let apiData = { view, history, gameDays: gameDaysOff }

vi.mock('../hooks/useAPI', () => ({
  useAPI: path => {
    if (path?.startsWith('/api/v1/sre/autonomy/history')) {
      return { data: apiData.history, loading: false, error: null, refetch }
    }
    if (path?.startsWith('/api/v1/sre/autonomy/game-days')) {
      return { data: apiData.gameDays, loading: false, error: null, refetch }
    }
    if (path?.startsWith('/api/v1/sre/autonomy/rollouts')) {
      return { data: { items: [] }, loading: false, error: null, refetch }
    }
    return { data: apiData.view, loading: false, error: null, refetch }
  },
}))

const admin = { role: 'admin', email: 'admin@example.com' }
const operator = { role: 'operator', email: 'ops@example.com' }
const viewer = { role: 'viewer', email: 'viewer@example.com' }

describe('AutonomyPage', () => {
  beforeEach(() => {
    apiData = { view, history, gameDays: gameDaysOff }
    globalThis.fetch = vi.fn(() => Promise.resolve({
      ok: true, status: 200, json: () => Promise.resolve({ ok: true }),
    }))
  })
  afterEach(() => vi.restoreAllMocks())

  it('lists every family and class with its levels', () => {
    render(<AutonomyPage database="orders" user={viewer} />)
    const wal = screen.getByTestId('autonomy-row-wal_retention-wal_bound')
    expect(within(wal).getByText('wal_bound')).toBeInTheDocument()
    expect(within(wal).getByTestId('granted')).toHaveTextContent('L1')
    expect(within(wal).getByTestId('cap')).toHaveTextContent('L2')
    expect(within(wal).getByTestId('effective')).toHaveTextContent('L1')
    expect(within(wal).getByText(/ha_role_not_primary/)).toBeInTheDocument()
    expect(screen.getByTestId('autonomy-row-lock_blocking-backend_cancel'))
      .toBeInTheDocument()
    expect(screen.getByText(/Enforced/)).toBeInTheDocument()
    expect(screen.getByText('promotion_proposed')).toBeInTheDocument()
  })

  it('shows the unmet evidence for the next level', () => {
    render(<AutonomyPage database="orders" user={viewer} />)
    const row = screen.getByTestId('autonomy-row-lock_blocking-backend_cancel')
    expect(within(row).getByText(/shadow_volume/)).toBeInTheDocument()
    expect(within(row).getByText(/>= 20 reviewed packets/)).toBeInTheDocument()
  })

  it('lets only an admin approve and only operators or admins act', () => {
    const { unmount } = render(<AutonomyPage database="orders" user={viewer} />)
    expect(screen.queryByRole('button', { name: /approve/i })).toBeNull()
    expect(screen.queryByRole('button', { name: /reject/i })).toBeNull()
    expect(screen.queryByRole('button', { name: /downgrade/i })).toBeNull()
    unmount()
    const op = render(<AutonomyPage database="orders" user={operator} />)
    expect(screen.queryByRole('button', { name: /approve/i })).toBeNull()
    expect(screen.getByRole('button', { name: /reject/i })).toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: /downgrade/i }).length).toBeGreaterThan(0)
    op.unmount()
    render(<AutonomyPage database="orders" user={admin} />)
    expect(screen.getByRole('button', { name: /approve/i })).toBeInTheDocument()
  })

  it('approves a proposal for the selected database', async () => {
    render(<AutonomyPage database="orders" user={admin} />)
    fireEvent.change(screen.getByLabelText(/approval note/i),
      { target: { value: 'replay reviewed' } })
    fireEvent.click(screen.getByRole('button', { name: /approve/i }))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
    const [url, opts] = globalThis.fetch.mock.calls[0]
    expect(url).toBe('/api/v1/sre/autonomy/proposals/'
      + '11111111-1111-4111-8111-111111111111/approve?database=orders')
    expect(opts.method).toBe('POST')
    expect(opts.headers['Content-Type']).toBe('application/json')
    expect(JSON.parse(opts.body)).toEqual({ note: 'replay reviewed' })
    await waitFor(() => expect(refetch).toHaveBeenCalled())
  })

  it('downgrades a pair with a reason', async () => {
    render(<AutonomyPage database="orders" user={operator} />)
    const row = screen.getByTestId('autonomy-row-wal_retention-wal_bound')
    fireEvent.change(within(row).getByLabelText(/downgrade to/i), { target: { value: 'L0' } })
    fireEvent.change(within(row).getByLabelText(/reason/i),
      { target: { value: 'replica lag drill' } })
    fireEvent.click(within(row).getByRole('button', { name: /downgrade/i }))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
    const [url, opts] = globalThis.fetch.mock.calls[0]
    expect(url).toBe('/api/v1/sre/autonomy/downgrade?database=orders')
    expect(JSON.parse(opts.body)).toEqual({
      family: 'wal_retention', class: 'wal_bound', level: 'L0', reason: 'replica lag drill',
    })
  })

  it('does not send a downgrade without a reason', () => {
    render(<AutonomyPage database="orders" user={operator} />)
    const row = screen.getByTestId('autonomy-row-wal_retention-wal_bound')
    fireEvent.click(within(row).getByRole('button', { name: /downgrade/i }))
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })

  it('surfaces a failed action', async () => {
    globalThis.fetch = vi.fn(() => Promise.resolve({
      ok: false, status: 409,
      json: () => Promise.resolve({
        error: 'promotion evidence is not met', code: 'evidence_not_met',
      }),
    }))
    render(<AutonomyPage database="orders" user={admin} />)
    fireEvent.click(screen.getByRole('button', { name: /approve/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/evidence is not met/)
  })

  // Coordinator decision 2026-10-02: autonomy pg_sage had before M7 is
  // carried over, and the page says so with the decision that granted it.
  it('marks a carried-over level with its decision', () => {
    render(<AutonomyPage database="orders" user={viewer} />)
    const row = screen.getByTestId('autonomy-row-wraparound_runway-freeze')
    expect(within(row).getByTestId('granted')).toHaveTextContent('L3')
    expect(within(row).getByText(/carried over/i)).toBeInTheDocument()
    expect(within(row).getByText(/spec F3/)).toBeInTheDocument()
    const earned = screen.getByTestId('autonomy-row-lock_blocking-backend_cancel')
    expect(within(earned).queryByText(/carried over/i)).toBeNull()
  })

  it('warns when enforcement is off', () => {
    apiData = { view: { ...view, enforced: false }, history, gameDays: gameDaysOff }
    render(<AutonomyPage database="orders" user={viewer} />)
    expect(screen.getByText(/not enforced/i)).toBeInTheDocument()
  })

  // benches-08: a game day shows what it scored; one that scored nothing
  // is a warning, distinct from game days being off.
  const gameDaysOn = {
    enabled: true,
    items: [
      {
        id: 'gd-unscored', status: 'completed_unscored', started_at: '2026-10-05T03:00:00Z',
        families: ['wal_retention', 'lock_blocking'],
        eval_run_id: '22222222-2222-4222-8222-222222222222',
        summary: {
          scored: 0, skipped: 7, errored: 0,
          top_skips: [
            { reason: 'needs superuser (CHECKPOINT)', count: 5 },
            { reason: 'max_prepared_transactions is 0', count: 2 },
          ],
        },
      },
      {
        id: 'gd-scored', status: 'completed', started_at: '2026-09-28T03:00:00Z',
        families: [], eval_run_id: '33333333-3333-4333-8333-333333333333',
        summary: { scored: 9, skipped: 1, errored: 0,
          top_skips: [{ reason: 'max_prepared_transactions is 0', count: 1 }] },
      },
      { id: 'gd-legacy', status: 'completed', started_at: '2026-09-21T03:00:00Z',
        families: [] },
    ],
  }

  it('shows what each game day scored and why runs were skipped', () => {
    apiData = { view, history, gameDays: gameDaysOn }
    render(<AutonomyPage database="orders" user={viewer} />)
    const unscored = screen.getByTestId('game-day-gd-unscored')
    expect(within(unscored).getByText(/scored 0 \/ skipped 7/)).toBeInTheDocument()
    expect(within(unscored).getByText(/needs superuser \(CHECKPOINT\) \(5\)/))
      .toBeInTheDocument()
    expect(within(unscored).getByText(/max_prepared_transactions is 0 \(2\)/))
      .toBeInTheDocument()
    expect(within(unscored).getByText(/22222222-2222-4222-8222-222222222222/))
      .toBeInTheDocument()
    const scored = screen.getByTestId('game-day-gd-scored')
    expect(within(scored).getByText(/scored 9 \/ skipped 1/)).toBeInTheDocument()
    expect(within(scored).queryByRole('alert')).toBeNull()
    const legacy = screen.getByTestId('game-day-gd-legacy')
    expect(within(legacy).queryByText(/scored/)).toBeNull()
    expect(screen.queryByText(/^Off:/)).toBeNull()
  })

  it('warns on a game day that scored nothing', () => {
    apiData = { view, history, gameDays: gameDaysOn }
    render(<AutonomyPage database="orders" user={viewer} />)
    const unscored = screen.getByTestId('game-day-gd-unscored')
    const status = within(unscored).getByTestId('game-day-status')
    expect(status).toHaveTextContent(/scored nothing/i)
    expect(status).toHaveAttribute('data-tone', 'warning')
    const ok = within(screen.getByTestId('game-day-gd-scored')).getByTestId('game-day-status')
    expect(ok).toHaveTextContent('completed')
    expect(ok).not.toHaveAttribute('data-tone', 'warning')
  })

  it('links a game day family to its game-day check on the path', () => {
    const climbing = {
      ...view,
      view: {
        ...view.view,
        families: view.view.families.map(f => f.family !== 'lock_blocking' ? f : {
          ...f,
          classes: f.classes.map(c => ({
            ...c,
            next: { ...c.next, checks: [...c.next.checks, {
              name: 'game_day_safe_pass', met: false, observed: '1/2',
              required: '>= 100% Safe Pass on game days, if any',
            }] },
          })),
        }),
      },
    }
    apiData = { view: climbing, history, gameDays: gameDaysOn }
    render(<AutonomyPage database="orders" user={viewer} />)
    const unscored = screen.getByTestId('game-day-gd-unscored')
    const link = within(unscored).getByRole('link', { name: 'lock_blocking' })
    expect(link).toHaveAttribute('href', '#path-lock_blocking-backend_cancel')
    expect(document.getElementById('path-lock_blocking-backend_cancel'))
      .not.toBeNull()
    expect(within(unscored).queryByRole('link', { name: 'wal_retention' })).toBeNull()
    expect(within(unscored).getByText(/wal_retention/)).toBeInTheDocument()
  })

  it('still says game days are off when they are', () => {
    render(<AutonomyPage database="orders" user={viewer} />)
    expect(screen.getByText(/Off: needs sre.autonomy.game_days/)).toBeInTheDocument()
    expect(screen.queryByTestId('game-day-status')).toBeNull()
  })
})
