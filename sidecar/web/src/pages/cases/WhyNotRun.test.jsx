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
      human_reason: 'outside maintenance window' }] })
    render(<WhyNotRun caseRow={findingCase} user={operator} />)
    fireEvent.click(screen.getByRole('button', { name: /why hasn't this run/i }))
    expect(await screen.findByTestId('why-not-run-result')).toHaveTextContent(
      'create_index: blocked, outside maintenance window')
    expect(apiGet).toHaveBeenCalledWith('/api/v1/policy/explain?database=alpha&finding_id=17')
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
