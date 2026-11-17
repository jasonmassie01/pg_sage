import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { CasePlans } from './CasePlans'

// ux-03: a case whose findings are steps of a plan links to the plan view.
// No concurrent access tests here: a pure render of the case payload.

describe('CasePlans', () => {
  it('links each plan by its goal to the plan view in the case database', () => {
    render(<CasePlans database="orders" plans={[
      { id: 7, goal: 'cut blocks per call on the order lookups' },
      { id: 9, goal: '<b>not markup</b>' }]} />)
    const links = screen.getAllByRole('link')
    expect(links.map(a => a.textContent)).toEqual([
      'Part of plan: cut blocks per call on the order lookups',
      'Part of plan: <b>not markup</b>'])
    expect(links.map(a => a.getAttribute('href'))).toEqual([
      '#/actions?tab=plans&plan=7&database=orders',
      '#/actions?tab=plans&plan=9&database=orders'])
  })

  it('renders nothing without plans, and skips a plan with no usable id', () => {
    const { container, rerender } = render(<CasePlans database="orders" plans={[]} />)
    expect(container).toBeEmptyDOMElement()
    rerender(<CasePlans database="orders" plans={undefined} />)
    expect(container).toBeEmptyDOMElement()
    rerender(<CasePlans database="orders" plans={[{ id: 0, goal: 'x' },
      { id: 'abc', goal: 'y' }, { id: 3 }]} />)
    expect(screen.getAllByRole('link').map(a => a.textContent))
      .toEqual(['Part of plan: plan 3'])
  })
})
