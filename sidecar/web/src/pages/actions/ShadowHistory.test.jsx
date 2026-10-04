import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ShadowHistory } from './ShadowHistory'

// Roadmap 1.4: the approval card says when its class has shadow history
// and whether this proposal itself was recorded as a shadow decision, so
// the operator knows the decision will score it.

const history = {
  family: 'tuning', class: 'index_create',
  summary: { total: 12, pending: 3, correct: 6, incorrect: 2, neutral: 0, unscored: 1,
    counted: 5 },
  this_proposal: { id: 7, status: 'pending', trusted_verdict: 'execute' },
}

describe('ShadowHistory', () => {
  it('shows the class history and that this decision scores the shadow', () => {
    render(<ShadowHistory history={history} />)
    const el = screen.getByTestId('approval-shadow')
    expect(el).toHaveTextContent(/12 shadow decisions for index_create/)
    expect(el).toHaveTextContent('6 correct')
    expect(el).toHaveTextContent('2 incorrect')
    expect(el).toHaveTextContent(/your decision scores it/i)
  })

  it('omits the proposal note when the proposal was not shadowed', () => {
    render(<ShadowHistory history={{ ...history, this_proposal: null }} />)
    expect(screen.getByTestId('approval-shadow')).not.toHaveTextContent(/scores it/i)
  })

  it('renders nothing without history', () => {
    const { container } = render(<ShadowHistory history={null} />)
    expect(container).toBeEmptyDOMElement()
  })
})
