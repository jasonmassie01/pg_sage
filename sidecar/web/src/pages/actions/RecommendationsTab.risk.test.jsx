import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { RecommendationsTab } from './RecommendationsTab'
import { riskLabel } from './workflow'

// rules-07: the Actions recommendations tab is the live surface that shows
// a finding's risk. It labels the normalized tiers, the advisory "none",
// and shows the executor contract tier and why approval is needed.

const subsetDrop = {
  id: 'f1', state: 'proposed', title: 'Subset index public.ix_sub',
  action_type: 'drop_unused_index', action_risk: 'high', contract_tier: 'moderate',
  approval_required: true,
  approval_reason: "high-risk actions always need an operator's approval",
}

function riskLine(row) {
  render(<RecommendationsTab database="prod" items={[row]} />)
  return screen.getByTestId('proposal-risk')
}

describe('RecommendationsTab risk', () => {
  it('labels a high-risk subset drop High with its contract and approval reason', () => {
    const line = riskLine(subsetDrop)
    expect(line.textContent).toContain('Risk: High')
    expect(line.textContent).not.toMatch(/high risk|high_risk/)
    expect(within(line).getByTestId('proposal-contract').textContent)
      .toBe('Specialist limit: Moderate')
    expect(screen.getByTestId('proposal-approval').textContent)
      .toBe("Needs approval: high-risk actions always need an operator's approval")
  })

  it('shows an empty-SQL finding as advisory only', () => {
    const line = riskLine({ id: 'f2', state: 'proposed', title: 'WAL advice',
      action_risk: 'none', contract_tier: 'none', approval_required: false,
      approval_reason: '' })
    expect(line.textContent).toContain('Risk: Advisory only')
    expect(within(line).getByTestId('proposal-contract').textContent)
      .toBe('Specialist limit: none (advisory only)')
    expect(screen.queryByTestId('proposal-approval')).toBeNull()
  })

  it('omits the contract tier for rows that do not carry one', () => {
    const line = riskLine({ id: 'f3', state: 'proposed', title: 'Analyze',
      risk_tier: 'safe' })
    expect(line.textContent).toContain('Risk: Low')
    expect(screen.queryByTestId('proposal-contract')).toBeNull()
    expect(screen.queryByTestId('proposal-approval')).toBeNull()
  })

  it('does not show an approval line when approval is not required', () => {
    riskLine({ ...subsetDrop, approval_required: false })
    expect(screen.queryByTestId('proposal-approval')).toBeNull()
  })
})

// The live rows are case candidates and stored recommendations: their
// approval comes from the policy explainer as requires_approval.
describe('RecommendationsTab approval from the case policy', () => {
  const approvalText = 'Needs approval: an operator must approve this change before it runs.'

  it('shows the approval line for a case candidate that requires approval', () => {
    riskLine({ id: 'c1', state: 'proposed', action_type: 'alter_system_guc',
      risk_tier: 'moderate', requires_approval: true })
    expect(screen.getByTestId('proposal-approval').textContent).toBe(approvalText)
  })

  it('reads the nested policy decision too', () => {
    riskLine({ id: 'c2', state: 'proposed', action_type: 'alter_system_guc',
      risk_tier: 'moderate', policy_decision: { requires_approval: true } })
    expect(screen.getByTestId('proposal-approval').textContent).toBe(approvalText)
  })

  it('uses the current candidate of a stored recommendation', () => {
    riskLine({ id: 'r1', state: 'proposed', storedRecommendation: true,
      action_type: 'drop_unused_index',
      currentCandidate: { action_type: 'drop_unused_index', risk_tier: 'high',
        requires_approval: true } })
    expect(screen.getByTestId('proposal-approval').textContent).toBe(approvalText)
  })

  it('shows no approval line when the policy does not require one', () => {
    riskLine({ id: 'c3', state: 'proposed', action_type: 'analyze_table',
      risk_tier: 'safe', requires_approval: false,
      policy_decision: { requires_approval: false } })
    expect(screen.queryByTestId('proposal-approval')).toBeNull()
  })
})

describe('riskLabel', () => {
  it('maps every normalized tier', () => {
    expect(riskLabel('safe')).toBe('Low')
    expect(riskLabel('moderate')).toBe('Moderate')
    expect(riskLabel('high')).toBe('High')
    expect(riskLabel('high_risk')).toBe('High')
    expect(riskLabel('none')).toBe('Advisory only')
    expect(riskLabel('read_only')).toBe('Read only')
  })

  it('treats empty and missing values as unknown', () => {
    expect(riskLabel('')).toBe('Unknown')
    expect(riskLabel(undefined)).toBe('Unknown')
    expect(riskLabel(null)).toBe('Unknown')
  })
})
