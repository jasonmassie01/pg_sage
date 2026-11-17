import { fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { WhyNotRun } from './WhyNotRun'

// policy-07: a case candidate's "Why hasn't this run?" asks the standing
// gate to explain the finding's candidates (GET /api/v1/policy/explain),
// which records nothing, and shows each verdict with its human reason.

const apiGet = vi.fn()
vi.mock('../../lib/api', () => ({ apiGet: (...args) => apiGet(...args) }))

const findingCase = {
  source_type: 'finding', source_ids: ['17'], database_name: 'alpha',
  action_candidates: [{ action_type: 'create_index' }],
}
const operator = { role: 'operator' }

beforeEach(() => { apiGet.mockReset() })

describe('WhyNotRun', () => {
  it('explains the finding candidates on request', async () => {
    apiGet.mockResolvedValue({ status: 'explained', candidates: [{
      action_type: 'create_index', decision: 'blocked',
      blocked_reason: 'outside_maintenance_window',
      human_reason: 'outside maintenance window' }] })
    render(<WhyNotRun caseRow={findingCase} user={operator} />)
    fireEvent.click(screen.getByRole('button', { name: /why hasn't this run/i }))
    expect(await screen.findByTestId('why-not-run-result')).toHaveTextContent(
      'Add an index: Blocked. Outside the maintenance window.')
    expect(apiGet).toHaveBeenCalledWith('/api/v1/policy/explain?database=alpha&finding_id=17')
  })

  // ux-05b win 7: each line names the action and the verdict in words and
  // the reason as blockerText words it, with the gate's detail.
  async function explainLines(candidates) {
    apiGet.mockResolvedValue({ status: 'explained', candidates })
    const view = render(<WhyNotRun caseRow={findingCase} user={operator} />)
    fireEvent.click(screen.getByRole('button', { name: /why hasn't this run/i }))
    const result = await screen.findByTestId('why-not-run-result')
    return { view, lines: [...result.querySelectorAll('p')].map(p => p.textContent) }
  }

  it('words each candidate without raw enums', async () => {
    const { lines } = await explainLines([
      { action_type: 'vacuum_table', decision: 'queue_for_approval',
        blocked_reason: 'approval_required', human_reason: 'approval required' },
      { action_type: 'analyze', decision: 'parked', blocked_reason: 'budget_exceeded',
        detail: 'next frees at 18:00', human_reason: 'budget exceeded (next frees at 18:00)' },
      { action_type: 'reindex', decision: 'blocked', blocked_reason: 'emergency_stop',
        human_reason: 'emergency stop' },
    ])
    expect(lines).toHaveLength(3)
    expect(lines[0]).toMatch(/^Clean up expired table rows: Waiting for approval\. /)
    expect(lines[1]).toMatch(/^Update query-planning statistics: Parked\. Change budget/)
    expect(lines[1]).toContain('(next frees at 18:00)')
    expect(lines[2]).toBe('Rebuild an index: Blocked. Emergency stop is on. ' +
      'Release it in the header to resume.')
    for (const line of lines) expect(line).not.toMatch(/\b[a-z0-9]+_[a-z0-9_]+\b/)
  })

  it('says no reason was recorded for a withheld candidate without one', async () => {
    const { lines } = await explainLines([
      { action_type: 'create_index', decision: 'blocked', human_reason: '' }])
    expect(lines).toEqual(['Add an index: Blocked. No reason recorded.'])
  })

  it('adds no reason to a candidate the gate would run', async () => {
    const { lines } = await explainLines([
      { action_type: 'create_index', decision: 'execute', human_reason: '' }])
    expect(lines).toEqual(['Add an index: Allowed to run.'])
  })

  it('keeps a case reason written as a sentence', async () => {
    const { lines } = await explainLines([{ action_type: 'create_index', decision: 'blocked',
      blocked_reason: 'live DDL preflight has warnings; review required',
      human_reason: 'live DDL preflight has warnings; review required' }])
    expect(lines).toEqual(
      ['Add an index: Blocked. live DDL preflight has warnings; review required'])
  })

  it('renders an unknown verdict and action as text, never as markup', async () => {
    const { view, lines } = await explainLines([{ action_type: 'set_fillfactor',
      decision: '<img src=x>', blocked_reason: 'new_gate_reason' }])
    expect(lines).toEqual(
      ['Set fillfactor: Unknown verdict (<img src=x>). Unknown reason (new_gate_reason)'])
    expect(view.container.querySelector('img')).toBeNull()
  })

  it('says when the finding has nothing to run', async () => {
    apiGet.mockResolvedValue({ status: 'no_candidate', candidates: [] })
    render(<WhyNotRun caseRow={findingCase} user={operator} />)
    fireEvent.click(screen.getByRole('button', { name: /why hasn't this run/i }))
    expect(await screen.findByTestId('why-not-run-result')).toHaveTextContent(
      /no action to run/i)
  })

  it('shows the error when the explanation fails', async () => {
    apiGet.mockRejectedValue(new Error('finding not found'))
    render(<WhyNotRun caseRow={findingCase} user={operator} />)
    fireEvent.click(screen.getByRole('button', { name: /why hasn't this run/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent('finding not found')
  })

  it('is not offered to viewers, incidents or cases without a candidate', () => {
    const { rerender, container } = render(
      <WhyNotRun caseRow={findingCase} user={{ role: 'viewer' }} />)
    expect(container).toBeEmptyDOMElement()
    rerender(<WhyNotRun caseRow={{ ...findingCase, source_type: 'incident' }} user={operator} />)
    expect(container).toBeEmptyDOMElement()
    rerender(<WhyNotRun caseRow={{ ...findingCase, action_candidates: [] }} user={operator} />)
    expect(container).toBeEmptyDOMElement()
    expect(apiGet).not.toHaveBeenCalled()
  })
})
