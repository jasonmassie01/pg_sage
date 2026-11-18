import { fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { CaseWhyNot } from './CaseWhyNot'

// ux-08b: a finding's case opens the Why not panel for that finding (it
// replaces policy-07's "Why hasn't this run?"), even when the finding has
// no candidate: "nothing was even proposed" is the commonest answer.
// No concurrency test: one read per open.

const apiGet = vi.fn()
vi.mock('../../lib/api', () => ({ apiGet: (...args) => apiGet(...args) }))

const findingCase = { source_type: 'finding', source_ids: ['17'], database_name: 'alpha',
  action_candidates: [] }
const operator = { role: 'operator' }

beforeEach(() => {
  apiGet.mockReset()
  apiGet.mockResolvedValue({ database: 'alpha', failing: [], links: [] })
})

describe('CaseWhyNot', () => {
  it('opens the panel for the case finding, with or without a candidate', async () => {
    render(<CaseWhyNot caseRow={findingCase} user={operator} />)
    fireEvent.click(screen.getByRole('button', { name: 'Why not?' }))
    expect(await screen.findByTestId('why-not-panel')).toBeInTheDocument()
    expect(apiGet).toHaveBeenCalledWith('/api/v1/whynot?database=alpha&finding_id=17',
      expect.anything())
  })

  it('is not offered to viewers, incidents or cases without a finding id', () => {
    const { rerender, container } = render(
      <CaseWhyNot caseRow={findingCase} user={{ role: 'viewer' }} />)
    expect(container).toBeEmptyDOMElement()
    rerender(<CaseWhyNot caseRow={{ ...findingCase, source_type: 'incident' }}
      user={operator} />)
    expect(container).toBeEmptyDOMElement()
    rerender(<CaseWhyNot caseRow={{ ...findingCase, source_ids: [] }} user={operator} />)
    expect(container).toBeEmptyDOMElement()
    rerender(<CaseWhyNot caseRow={{ ...findingCase, source_ids: ['x1'] }} user={operator} />)
    expect(container).toBeEmptyDOMElement()
    expect(apiGet).not.toHaveBeenCalled()
  })
})
