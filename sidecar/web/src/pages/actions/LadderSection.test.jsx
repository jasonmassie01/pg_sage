import { render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { LadderSection, ladderLine, ledgerTierLabel } from './LadderSection'
import { ApprovalCard } from './ApprovalCard'
import { LedgerPage } from '../LedgerPage'

vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() }),
}))

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))
vi.mock('../../hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))

// lab-03a: the approval card shows what the change was tested on before
// production, one line per tier, and the ledger row its highest tier.
// No concurrency tests: these are pure renders of one card or row.

const planOnly = {
  tier: 1, name: 'plan_only', method: 'hypopg_hide', result: 'passed', predicted: -41,
  metric: 'plan_cost', evidence: { measured: 4 }, at: '2026-11-16T09:00:00Z',
}
const clonePassed = {
  tier: 2, name: 'clone', method: 'clone_rehearsal', result: 'passed', predicted: -41,
  metric: 'blocks_per_call', evidence: { receipt_id: 812 }, at: '2026-11-16T10:00:00Z',
}
const cloneSkipped = {
  tier: 2, name: 'clone', result: 'skipped', at: '2026-11-16T09:00:00Z',
  skipped: [{ tier: 2, reason: 'no clone provider configured' }],
}

describe('ladderLine', () => {
  it('words a passed clone test with its number and receipt', () => {
    expect(ladderLine(clonePassed))
      .toBe('Tested on a clone: blocks per call -41% (receipt 812)')
  })
  it('words a plan-only pass and a skipped tier', () => {
    expect(ladderLine(planOnly)).toBe('Tested on plans only: planned cost -41%')
    expect(ladderLine(cloneSkipped)).toBe('Skipped: no clone provider configured')
  })
  it('words a failed and an inconclusive tier with the reason', () => {
    expect(ladderLine({ tier: 2, name: 'clone', result: 'failed',
      evidence: { reason: 'blocks per call rose 12%' } }))
      .toBe('Tested on a clone: failed (blocks per call rose 12%)')
    expect(ladderLine({ tier: 1, name: 'plan_only', result: 'inconclusive',
      skipped: [{ tier: 1, reason: 'partial: 1 of 3 planned' }] }))
      .toBe('Tested on plans only: inconclusive (partial: 1 of 3 planned)')
  })
  it('signs a positive change and survives a malformed record', () => {
    expect(ladderLine({ ...planOnly, predicted: 3.25 }))
      .toBe('Tested on plans only: planned cost +3.3%')
    expect(ladderLine({ tier: 9, result: 'passed' })).toBe('Tier 9: passed')
    expect(ladderLine(null)).toBe('')
  })
})

describe('LadderSection', () => {
  it('renders each tier in order', () => {
    render(<LadderSection ladder={[planOnly, cloneSkipped]} />)
    const items = within(screen.getByTestId('approval-ladder')).getAllByRole('listitem')
    expect(items.map(i => i.textContent)).toEqual([
      'Tested on plans only: planned cost -41%', 'Skipped: no clone provider configured'])
  })
  it('says no test was recorded for a row from before the ladder', () => {
    render(<LadderSection ladder={undefined} note="No pre-production test recorded" />)
    expect(screen.getByTestId('approval-ladder')).toHaveTextContent(
      'No pre-production test recorded')
    expect(screen.queryAllByRole('listitem')).toHaveLength(0)
  })
  it('renders nothing for an unreadable ladder (no records, no note)', () => {
    const { container } = render(<LadderSection ladder={{ tier: 1 }} />)
    expect(container.innerHTML).toBe('')
  })
})

describe('ApprovalCard ladder line', () => {
  const base = {
    queue_id: 7, database: 'orders', title: 'Drop unused index public.idx_old',
    action_type: 'drop_unused_index', status: 'pending', targets: [], evidence: [],
    rationale: null, predicted_effect: {}, sql: 'DROP INDEX CONCURRENTLY public.idx_old',
    rollback: { class: 'forward_fix_only', sql: '' },
    risk: { tier: 'moderate', blast_radius: '', lock: '', guardrails: [], post_checks: [] },
    why_approval: [{ code: 'not_rehearsed', text: 'not rehearsed on a clone' }],
    proposed_at: '2026-11-16T09:00:00Z', expires_at: '2099-01-01T00:00:00Z',
    card_hash: 'h',
  }
  it('shows the tested tiers on the card', () => {
    render(<ApprovalCard card={{ ...base, ladder: [planOnly, cloneSkipped] }}
      onDecided={() => {}} />)
    expect(screen.getByTestId('approval-ladder')).toHaveTextContent(
      'Skipped: no clone provider configured')
  })
  it('shows the none-recorded note', () => {
    render(<ApprovalCard card={{ ...base,
      ladder_note: 'No pre-production test recorded' }} onDecided={() => {}} />)
    expect(screen.getByTestId('approval-ladder')).toHaveTextContent(
      'No pre-production test recorded')
  })
})

describe('ledger tier', () => {
  it('labels the highest tier passed', () => {
    expect(ledgerTierLabel('clone')).toBe('Tested on a clone')
    expect(ledgerTierLabel('plan_only')).toBe('Tested on plans only')
    expect(ledgerTierLabel('')).toBe('')
    expect(ledgerTierLabel(undefined)).toBe('')
  })
  it('shows the tier on the ledger row', () => {
    const row = {
      action_id: 12, database: 'orders', action_type: 'drop_index', what: 'Drop an index',
      why: '', origin: 'autonomous', expected: null, measured: null, measured_reason: 'x',
      verdict: 'pending', rollback_state: 'available', tier: 'clone',
      executed_at: '2026-11-16T03:00:00Z',
    }
    api.useAPI.mockReturnValue({ data: { since: '24h', rows: [row], truncated: false,
      errors: [], summary: { changes: 1, measured: 0, measured_share: 0 } },
    loading: false, error: null, refetch: vi.fn() })
    render(<LedgerPage database="all" />)
    expect(screen.getByTestId('ledger-tier-orders-12')).toHaveTextContent('Tested on a clone')
  })
})
