import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { PlanStageRow } from './PlanStageRow'
import { planStep } from './planTestData'

// ux-03: one stage of a plan. No concurrent access tests here: the row
// sends nothing itself; PlanApproval.test.jsx owns the busy state.

function row(props) {
  render(<ul><PlanStageRow edges={[]} next={null} {...props} /></ul>)
  return screen.getByTestId(`plan-stage-${props.step.ordinal}`)
}

describe('PlanStageRow', () => {
  it('shows status, evidence links, exact SQL, effect, tier, gate and rollback', () => {
    const step = planStep(70, 1, 'idx_a', { evidence_ids: ['ev-1', 'ev 2'],
      expected_pct: 30, runtime: { state: 'kept', verdict: 'improved', action_log_id: 9 } })
    const el = row({ step, edges: [{ from: 1, to: 2 }, { from: 2, to: 3 }],
      next: { covered: true, needs_own_approval: false },
      outcome: { action_id: 9, verdict: 'improved', tier: 'clone',
        measured: { metric: 'blocks_per_call', change_pct: -31 } } })
    expect(el).toHaveTextContent('Stage 1')
    expect(within(el).getByTestId('plan-stage-status-1')).toHaveTextContent('Verified: kept')
    const links = within(el).getAllByRole('link', { name: /^ev/ })
    expect(links.map(a => a.getAttribute('href')))
      .toEqual(['#/ledger?evidence=ev-1', '#/ledger?evidence=ev+2'])
    expect(within(el).getByTestId('plan-stage-sql-1').tagName).toBe('PRE')
    expect(within(el).getByTestId('plan-stage-sql-1'))
      .toHaveTextContent('CREATE INDEX CONCURRENTLY idx_a ON public.orders (customer_id)')
    expect(el).toHaveTextContent('Expected: blocks per call down 30%')
    expect(el).toHaveTextContent('Tested on a clone')
    expect(el).toHaveTextContent('Measured: blocks per call -31%')
    expect(el).toHaveTextContent('Decision: Improved')
    expect(el).toHaveTextContent(/Gate: Automatic/)
    expect(within(el).getByTestId('plan-stage-rollback-1'))
      .toHaveTextContent('DROP INDEX CONCURRENTLY public.idx_a')
    expect(el).toHaveTextContent('Reverting it also reverts stages 2, 3, which depend on it')
  })

  it('renders SQL with </script> as text inside <pre>', () => {
    const sql = "SELECT '</script><script>window.hacked=1</script>'"
    const el = row({ step: planStep(70, 1, 'x', { proposed_sql: sql }) })
    const pre = within(el).getByTestId('plan-stage-sql-1')
    expect(pre.textContent).toBe(sql)
    expect(pre.querySelector('script')).toBeNull()
    expect(window.hacked).toBeUndefined()
  })

  it('says not measured yet before a ledger row exists, and no verdict', () => {
    const el = row({ step: planStep(70, 1, 'idx_a') })
    expect(el).toHaveTextContent('Measured: not measured yet')
    expect(el).not.toHaveTextContent('Verdict:')
    expect(el).toHaveTextContent('Tested on plans only')
    expect(el).not.toHaveTextContent('also reverts')
  })

  it('shows an operator stage as You run this, with no rollback button', () => {
    const step = planStep(80, 2, 'x', { kind: 'operator', action_type: undefined,
      proposed_sql: undefined, rollback_sql: undefined, metric: 'readback',
      expected_direction: 'none', expected_source: 'none',
      operator_command: 'gcloud sql instances patch db --database-flags=work_mem=64MB',
      operator_revert: 'gcloud sql instances patch db --database-flags=work_mem=4MB',
      landing: { setting: 'work_mem', value: '64MB' },
      runtime: { state: 'awaiting_operator' } })
    const el = row({ step })
    expect(el).toHaveTextContent('You run this')
    expect(within(el).getByTestId('plan-stage-command-2').tagName).toBe('PRE')
    expect(within(el).getByTestId('plan-stage-command-2'))
      .toHaveTextContent('gcloud sql instances patch db --database-flags=work_mem=64MB')
    expect(el).toHaveTextContent('Observed landing: waiting for work_mem to read 64MB')
    expect(el).toHaveTextContent('pg_sage cannot revert this stage')
    expect(within(el).queryAllByRole('button')).toHaveLength(0)
    expect(within(el).queryByTestId('plan-stage-sql-2')).toBeNull()
  })

  it('says an operator stage landed once it did', () => {
    const step = planStep(80, 2, 'x', { kind: 'operator', operator_command: 'set it',
      landing: { setting: 'work_mem', value: '64MB' },
      runtime: { state: 'judging', landed_at: '2026-11-16T10:00:00Z' } })
    expect(row({ step })).toHaveTextContent('Observed landing: work_mem reads 64MB')
  })

  it('offers Approve this stage only when asked, and calls back with the stage', () => {
    const step = planStep(71, 2, 'idx_b')
    const onApprove = vi.fn()
    const el = row({ step, onApproveStep: onApprove })
    fireEvent.click(within(el).getByRole('button', { name: 'Approve stage 2' }))
    expect(onApprove).toHaveBeenCalledWith(step)
    const quiet = row({ step: planStep(72, 3, 'idx_c') })
    expect(within(quiet).queryByRole('button')).toBeNull()
  })

  it('disables Approve this stage while a decision is in flight', () => {
    const el = row({ step: planStep(71, 2, 'idx_b'), onApproveStep: () => {}, busy: true })
    expect(within(el).getByRole('button', { name: 'Approve stage 2' })).toBeDisabled()
  })
})
