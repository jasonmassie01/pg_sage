import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { TrustPage } from './TrustPage'

// Roadmap 1.2 (one trust system): one Trust page, database x family x
// class, with the level, the evidence counts (improved / neutral /
// regressed / rolled back / rejected), the last change and why, and the
// path to the next level. The ledger, not the time ramp, decides.

const refetch = vi.fn()

const ordersView = {
  database: 'orders',
  generated_at: '2026-10-03T12:00:00Z',
  floor: {
    known: true, start: '2026-09-01T00:00:00Z', observed: '32 days',
    required_l2: '192h0m0s', required_l3: '744h0m0s',
  },
  grandfathered: {
    database: 'orders', migrated_at: '2026-10-03T08:00:00Z',
    seeded: [{ family: 'hygiene', class: 'index_drop', level: 'L3' }],
  },
  rows: [
    {
      family: 'hygiene', kind: 'self_initiated', class: 'index_drop',
      outcome_class: 'index_drop', level: 'L2', effective: 'L2', cap: 'L3',
      reversibility: 'reversible', provenance: 'ledger',
      evidence: { improved: 4, neutral: 2, regressed: 1, rolled_back: 0, rejected: 0,
        insufficient: 1, unverifiable: 0 },
      last_change: {
        at: '2026-10-03T10:00:00Z', by: 'pg_sage', event: 'auto_downgraded',
        reason: 'regressed: action 41 (index_drop) on orders; L3 -> L2',
      },
      next: {
        target: 'L3', met: false,
        checks: [{
          name: 'class_successes', met: false, observed: '0', required: '>= 10',
          how: '10 more verified index_drop actions since the last demerit',
        }],
      },
    },
    {
      family: 'hygiene', kind: 'self_initiated', class: 'vacuum',
      outcome_class: 'vacuum', level: 'L3', effective: 'L1', cap: 'L3',
      reversibility: 'reversible', provenance: 'grandfathered',
      provenance_ref: 'trust.level=autonomous, tier3_safe, ramp 192h elapsed',
      downgrades: [{ reason: 'error_budget_fast_burn', detail: 'burning' }],
      evidence: { improved: 0, neutral: 9, regressed: 0, rolled_back: 0, rejected: 0 },
      last_change: { at: '2026-10-03T08:00:00Z', by: 'pg_sage', event: 'grandfathered',
        reason: 'grandfathered from the time ramp' },
    },
    {
      family: 'tuning', kind: 'self_initiated', class: 'index_create',
      outcome_class: 'index_create', level: 'L1', effective: 'L1', cap: 'L3',
      reversibility: 'reversible', provenance: 'ledger',
      evidence: { improved: 3, neutral: 0, regressed: 0, rolled_back: 0, rejected: 1 },
      last_change: {},
      pending: { id: '22222222-2222-4222-8222-222222222222', from: 'L1', to: 'L2',
        status: 'pending' },
      next: { target: 'L2', met: true, checks: [] },
    },
    {
      family: 'wraparound_runway', kind: 'incident', class: 'freeze',
      level: 'L3', effective: 'L3', cap: 'L3', reversibility: 'reversible',
      provenance: 'carried_over', provenance_ref: 'spec F3',
      evidence: { improved: 1, neutral: 0, regressed: 0, rolled_back: 0, rejected: 0 },
      last_change: { at: '2026-10-02T08:00:00Z', by: 'pg_sage', event: 'carried_over',
        reason: 'carried over' },
    },
    {
      family: 'hygiene', kind: 'self_initiated', class: 'retention',
      outcome_class: 'retention', level: 'L1', effective: 'L1', cap: 'L1',
      reversibility: 'irreversible', provenance: 'ledger',
      evidence: { improved: 0, neutral: 0, regressed: 0, rolled_back: 0, rejected: 0 },
      last_change: {},
    },
  ],
}

let apiState = {
  data: { enforced: true, meaning: 'trust.level is the ceiling; the ledger decides.',
    databases: [ordersView] },
  error: null,
}
let lastPath = null

vi.mock('../hooks/useAPI', () => ({
  useAPI: path => {
    lastPath = path
    return { data: apiState.data, loading: false, error: apiState.error, refetch }
  },
}))

const admin = { role: 'admin', email: 'admin@example.com' }
const viewer = { role: 'viewer', email: 'viewer@example.com' }

describe('TrustPage', () => {
  beforeEach(() => {
    apiState = {
      data: { enforced: true, meaning: 'trust.level is the ceiling; the ledger decides.',
        databases: [ordersView] },
      error: null,
    }
    globalThis.fetch = vi.fn(() => Promise.resolve({
      ok: true, status: 200, json: () => Promise.resolve({ ok: true }),
    }))
  })
  afterEach(() => vi.restoreAllMocks())

  it('shows every family and class of both kinds with its level', () => {
    render(<TrustPage database="all" user={viewer} />)
    expect(lastPath).toBe('/api/v1/trust')
    const drop = screen.getByTestId('trust-row-orders-hygiene-index_drop')
    expect(within(drop).getByTestId('trust-level')).toHaveTextContent('L2')
    expect(within(drop).getByTestId('trust-cap')).toHaveTextContent('L3')
    expect(screen.getByTestId('trust-row-orders-wraparound_runway-freeze'))
      .toBeInTheDocument()
    expect(screen.getByTestId('trust-section-orders-self_initiated')).toBeInTheDocument()
    expect(screen.getByTestId('trust-section-orders-incident')).toBeInTheDocument()
    expect(screen.getByText(/the ledger decides/)).toBeInTheDocument()
  })

  it('shows the model lift over deterministic (roadmap 2.4)', async () => {
    render(<TrustPage database="all" user={viewer} />)
    expect(await screen.findByTestId('model-lift')).toBeInTheDocument()
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/v1/model-lift',
      expect.objectContaining({ credentials: 'include' }))
  })

  it('shows the evidence counts', () => {
    render(<TrustPage database="all" user={viewer} />)
    const drop = screen.getByTestId('trust-row-orders-hygiene-index_drop')
    const ev = within(drop).getByTestId('trust-evidence')
    expect(ev).toHaveTextContent('4 improved')
    expect(ev).toHaveTextContent('2 neutral')
    expect(ev).toHaveTextContent('1 regressed')
    const create = screen.getByTestId('trust-row-orders-tuning-index_create')
    expect(within(create).getByTestId('trust-evidence')).toHaveTextContent('1 rejected')
  })

  it('shows the last change and why', () => {
    render(<TrustPage database="all" user={viewer} />)
    const drop = screen.getByTestId('trust-row-orders-hygiene-index_drop')
    const change = within(drop).getByTestId('trust-last-change')
    expect(change).toHaveTextContent('auto_downgraded')
    expect(change).toHaveTextContent(/regressed: action 41/)
    const create = screen.getByTestId('trust-row-orders-tuning-index_create')
    expect(within(create).getByTestId('trust-last-change')).toHaveTextContent(/default/i)
  })

  it('labels grandfathered levels with what granted them', () => {
    render(<TrustPage database="all" user={viewer} />)
    const vac = screen.getByTestId('trust-row-orders-hygiene-vacuum')
    expect(within(vac).getByTestId('trust-provenance')).toHaveTextContent('grandfathered')
    expect(within(vac).getByTestId('trust-provenance'))
      .toHaveAttribute('title', expect.stringContaining('ramp 192h elapsed'))
    expect(within(vac).getByTestId('trust-effective')).toHaveTextContent('L1')
    expect(within(vac).getByText(/error_budget_fast_burn/)).toBeInTheDocument()
    expect(screen.getByTestId('trust-grandfathered-orders'))
      .toHaveTextContent(/hygiene\/index_drop L3/)
  })

  it('shows the path to the next level', () => {
    render(<TrustPage database="all" user={viewer} />)
    const drop = screen.getByTestId('trust-row-orders-hygiene-index_drop')
    expect(within(drop).getByTestId('trust-next')).toHaveTextContent(
      /10 more verified index_drop actions/)
    const vac = screen.getByTestId('trust-row-orders-hygiene-vacuum')
    expect(within(vac).getByTestId('trust-next')).toHaveTextContent(/at its cap/i)
    const ret = screen.getByTestId('trust-row-orders-hygiene-retention')
    expect(within(ret).getByTestId('trust-next')).toHaveTextContent(/irreversible/i)
    const create = screen.getByTestId('trust-row-orders-tuning-index_create')
    expect(within(create).getByTestId('trust-next')).toHaveTextContent(/waiting for an admin/i)
  })

  it('asks for the selected database', () => {
    render(<TrustPage database="orders" user={viewer} />)
    expect(lastPath).toBe('/api/v1/trust?database=orders')
  })

  it('lets only an admin approve a pending promotion', async () => {
    const { unmount } = render(<TrustPage database="all" user={viewer} />)
    expect(screen.queryByRole('button', { name: /approve/i })).toBeNull()
    unmount()
    render(<TrustPage database="all" user={admin} />)
    fireEvent.click(screen.getByRole('button', { name: /approve/i }))
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
    const [url, opts] = globalThis.fetch.mock.calls[0]
    expect(url).toBe('/api/v1/sre/autonomy/proposals/'
      + '22222222-2222-4222-8222-222222222222/approve?database=orders')
    expect(opts.method).toBe('POST')
    await waitFor(() => expect(refetch).toHaveBeenCalled())
  })

  it('surfaces a failed approval', async () => {
    globalThis.fetch = vi.fn(() => Promise.resolve({
      ok: false, status: 409,
      json: () => Promise.resolve({ error: 'promotion evidence is not met' }),
    }))
    render(<TrustPage database="all" user={admin} />)
    fireEvent.click(screen.getByRole('button', { name: /approve/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/evidence is not met/)
  })

  it('warns when the ledger is not enforced', () => {
    apiState.data = { ...apiState.data, enforced: false }
    render(<TrustPage database="all" user={viewer} />)
    expect(screen.getByTestId('trust-not-enforced')).toHaveTextContent(
      /sre.autonomy.enforce: false/)
  })

  it('shows an error banner when the ledger cannot be read', () => {
    apiState = { data: null, error: '500 Internal Server Error' }
    render(<TrustPage database="all" user={viewer} />)
    expect(screen.getByText(/500 Internal Server Error/)).toBeInTheDocument()
  })

  it('explains an empty ledger list', () => {
    apiState.data = { enforced: true, meaning: '', databases: [] }
    render(<TrustPage database="all" user={viewer} />)
    expect(screen.getByTestId('trust-empty')).toBeInTheDocument()
  })
})
