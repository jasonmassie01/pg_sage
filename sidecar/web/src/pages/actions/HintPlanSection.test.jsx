import { render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { HintPlanSection, hintPlanSummary } from './HintPlanSection'
import { ApprovalCard } from './ApprovalCard'

vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() }),
}))

// tuning-05b: a query-hint card shows the plan comparison that gated the
// hint, with both plans in collapsed blocks.

const plainPlan = [{ Plan: { 'Node Type': 'Seq Scan', 'Relation Name': 'orders',
  'Total Cost': 412 } }]
const hintedPlan = [{ Plan: { 'Node Type': 'Index Scan', 'Index Name': 'idx_orders_customer',
  'Relation Name': 'orders', 'Total Cost': 254.6 } }]

function evidence(overrides = {}) {
  return {
    verdict: 'changed', reason: 'the hinted plan uses idx_orders_customer', basis: 'plan',
    plain_cost: 412, hinted_cost: 254.6, cost_delta_pct: -38.2,
    plain_plan_hash: 'a1b2', hinted_plan_hash: 'c3d4',
    plain_plan: plainPlan, hinted_plan: hintedPlan, generic_plan: false, pg_major: 17,
    ...overrides,
  }
}

describe('hintPlanSummary', () => {
  it('names the verdict, both costs and the signed delta', () => {
    expect(hintPlanSummary(evidence()))
      .toBe('Plan comparison: changed, cost 412.0 to 254.6 (-38.2%)')
  })

  it('signs a positive delta and keeps a zero delta', () => {
    expect(hintPlanSummary(evidence({ cost_delta_pct: 12 })))
      .toBe('Plan comparison: changed, cost 412.0 to 254.6 (+12.0%)')
    expect(hintPlanSummary(evidence({ cost_delta_pct: 0 })))
      .toBe('Plan comparison: changed, cost 412.0 to 254.6 (0.0%)')
  })

  it('renders no delta when none was stored (plain cost 0)', () => {
    const ev = evidence({ plain_cost: 0 })
    delete ev.cost_delta_pct
    expect(hintPlanSummary(ev)).toBe('Plan comparison: changed, cost 0.0 to 254.6')
  })

  it('shows the verdict alone when no plan was read', () => {
    expect(hintPlanSummary({ verdict: 'unverifiable', reason: 'pg_hint_plan not loaded' }))
      .toBe('Plan comparison: unverifiable')
  })
})

describe('HintPlanSection', () => {
  it('renders the summary and both plans collapsed as pretty JSON', () => {
    render(<HintPlanSection evidence={evidence()} />)
    const section = screen.getByTestId('approval-hint-plans')
    expect(within(section).getByTestId('hint-plan-summary').textContent)
      .toBe('Plan comparison: changed, cost 412.0 to 254.6 (-38.2%)')
    expect(within(section).queryByTestId('hint-plan-reason')).toBeNull()
    for (const [id, label, plan] of [['hint-plan-plain', 'Plan without hint', plainPlan],
      ['hint-plan-hinted', 'Plan with hint', hintedPlan]]) {
      const block = within(section).getByTestId(id)
      expect(block.tagName).toBe('DETAILS')
      expect(block.open).toBe(false)
      expect(within(block).getByText(label).tagName).toBe('SUMMARY')
      expect(block.querySelector('pre').textContent).toBe(JSON.stringify(plan, null, 2))
    }
  })

  it('shows the reason when the verdict is not changed', () => {
    render(<HintPlanSection evidence={evidence({ verdict: 'no_effect',
      reason: 'both plans are the same' })} />)
    expect(screen.getByTestId('hint-plan-reason').textContent)
      .toContain('both plans are the same')
  })

  it('shows verdict and reason with no plan blocks for unverifiable evidence', () => {
    render(<HintPlanSection evidence={{ verdict: 'unverifiable',
      reason: 'pg_hint_plan not loaded', generic_plan: true, pg_major: 14 }} />)
    expect(screen.getByTestId('hint-plan-summary').textContent)
      .toBe('Plan comparison: unverifiable')
    expect(screen.getByTestId('hint-plan-reason').textContent)
      .toContain('pg_hint_plan not loaded')
    expect(screen.queryByTestId('hint-plan-plain')).toBeNull()
    expect(screen.queryByTestId('hint-plan-hinted')).toBeNull()
  })

  it('shows the omitted text instead of an oversized plan', () => {
    const ev = evidence({ plain_plan_omitted: 'plan of 71680 bytes exceeds 65536' })
    delete ev.plain_plan
    render(<HintPlanSection evidence={ev} />)
    const block = screen.getByTestId('hint-plan-plain')
    expect(block.querySelector('pre')).toBeNull()
    expect(block.textContent).toContain('plan of 71680 bytes exceeds 65536')
    expect(screen.getByTestId('hint-plan-hinted').querySelector('pre')).not.toBeNull()
  })

  it('renders plan text as text, never as markup', () => {
    const hostile = [{ Plan: { Filter: '<img src=x onerror="window.pwned=1">' } }]
    const { container } = render(<HintPlanSection evidence={evidence({
      plain_plan: hostile, reason: '<b>bold</b>', verdict: 'no_effect' })} />)
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('b')).toBeNull()
    expect(screen.getByTestId('hint-plan-plain').querySelector('pre').textContent)
      .toContain('<img src=x')
  })

  it.each([[undefined], [null], [{}], [[]], ['changed'], [{ verdict: '' }]])(
    'renders nothing for %j', (ev) => {
      const { container } = render(<HintPlanSection evidence={ev} />)
      expect(container.innerHTML).toBe('')
    })

  it('ignores non-numeric costs instead of printing NaN', () => {
    render(<HintPlanSection evidence={evidence({ plain_cost: 'x', hinted_cost: null })} />)
    const text = screen.getByTestId('hint-plan-summary').textContent
    expect(text).not.toContain('NaN')
    expect(text).toBe('Plan comparison: changed (-38.2%)')
  })
})

describe('ApprovalCard with hint evidence', () => {
  function card(extra = {}) {
    return {
      queue_id: 7, database: 'orders', title: 'Hint for query 4242',
      action_type: 'apply_query_hint', status: 'pending', targets: [],
      evidence: [{ kind: 'finding', label: 'Finding #12', value: 'warning', ref: 'finding:12' }],
      rationale: null, predicted_effect: {}, sql: "INSERT INTO hint_plan.hints VALUES (1)",
      rollback: { class: 'reversible', sql: 'DELETE FROM hint_plan.hints', note: '' },
      risk: { tier: 'moderate', blast_radius: 'one query', lock: 'no locks',
        guardrails: [], post_checks: [] },
      why_approval: [], proposed_at: '2026-11-01T11:00:00Z',
      expires_at: new Date(Date.now() + 3600 * 1000).toISOString(), card_hash: 'h7',
      ...extra,
    }
  }

  it('shows the plan section under the evidence', () => {
    render(<ApprovalCard card={card({ hint_evidence: evidence() })} onDecided={() => {}} />)
    const ev = screen.getByTestId('approval-evidence')
    const plans = screen.getByTestId('approval-hint-plans')
    expect(ev.compareDocumentPosition(plans) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  it('shows no plan section without hint evidence', () => {
    render(<ApprovalCard card={card()} onDecided={() => {}} />)
    expect(screen.getByTestId('approval-evidence')).toBeTruthy()
    expect(screen.queryByTestId('approval-hint-plans')).toBeNull()
  })
})
