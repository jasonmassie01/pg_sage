import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { AnswerActions } from './AnswerActions'

// Ask Sage may propose a fact; it stays proposed until a person confirms
// it on the Facts page. No confirm control is ever rendered here.

describe('AnswerActions fact proposals', () => {
  it('shows a proposed fact with a link to the Facts page', () => {
    render(<AnswerActions actions={[{ kind: 'fact', id: '12', status: 'proposed',
      evidence_id: 'fact:12' }]} />)
    expect(screen.getByTestId('ask-action').textContent)
      .toContain('Fact proposed (#12)')
    const link = screen.getByRole('link')
    expect(link.getAttribute('href')).toBe('#/facts')
    expect(link.textContent).toContain('a person confirms it there')
    expect(screen.queryByRole('button')).toBeNull()
  })

  it('shows a refused fact in red with its reason and no link', () => {
    render(<AnswerActions actions={[{ kind: 'fact', status: 'refused',
      reason: 'the fact is rejected' }]} />)
    expect(screen.getByTestId('ask-action').textContent).toContain('Fact refused')
    expect(screen.getByText(/the fact is rejected/)).toBeTruthy()
    expect(screen.queryByRole('link')).toBeNull()
  })
})
