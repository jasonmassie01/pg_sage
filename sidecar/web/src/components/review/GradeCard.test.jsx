import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { GradeCard } from './GradeCard'
import { LetterBadge } from './LetterBadge'
import { gradedRun } from '../../test/reviewFixtures'

// review-01b: the overall letter, its coverage and the rubric version.
// No concurrent access tests: a pure render of its props.

describe('GradeCard', () => {
  it('shows the overall letter, coverage and rubric version', () => {
    render(<GradeCard run={gradedRun()} />)
    const card = screen.getByTestId('grade-card')
    expect(within(card).getByTestId('letter-badge')).toHaveTextContent('B')
    expect(card).toHaveTextContent('Coverage 95.2%')
    expect(card).toHaveTextContent('Rubric pg_sage-rubric/1')
    expect(card).not.toHaveTextContent('Partial')
  })

  it('says unknown with the reason when the overall has no letter', () => {
    const run = gradedRun()
    run.grade = { ...run.grade, overall: null, known_dimensions: 3 }
    render(<GradeCard run={run} />)
    const badge = screen.getByTestId('letter-badge')
    expect(badge).toHaveTextContent('Unknown: 3 of 8 dimensions known, 4 needed')
    expect(badge).not.toHaveTextContent(/^[A-F]$/)
  })

  it('refuses an invalid overall letter or score', () => {
    const run = gradedRun()
    run.grade = { ...run.grade, overall: 'Z' }
    const { unmount } = render(<GradeCard run={run} />)
    expect(screen.getByTestId('letter-badge')).toHaveTextContent('Unknown: invalid grade')
    unmount()
    run.grade = { ...run.grade, overall: 'A', overall_score: 140 }
    render(<GradeCard run={run} />)
    expect(screen.getByTestId('letter-badge')).toHaveTextContent('Unknown: invalid grade')
  })

  it('names a partial run, where a producer could not be read', () => {
    render(<GradeCard run={gradedRun(12, { status: 'partial' })} />)
    expect(screen.getByTestId('grade-card')).toHaveTextContent('Partial')
  })
})

describe('LetterBadge', () => {
  it('pairs each letter with text, never colour alone', () => {
    for (const letter of ['A', 'B', 'C', 'D', 'F']) {
      const { unmount } = render(<LetterBadge letter={letter} score={50} />)
      const badge = screen.getByTestId('letter-badge')
      expect(badge).toHaveTextContent(letter)
      expect(badge).toHaveAttribute('aria-label', `Grade ${letter}`)
      unmount()
    }
  })

  it('renders unknown as words, not a grey letter or an empty cell', () => {
    render(<LetterBadge letter={null} reason="coverage 10%, below 60%" />)
    const badge = screen.getByTestId('letter-badge')
    expect(badge).toHaveTextContent('Unknown: coverage 10%, below 60%')
    expect(badge.textContent.trim()).not.toBe('')
  })

  it('names unknown even with no reason given', () => {
    render(<LetterBadge letter={null} />)
    expect(screen.getByTestId('letter-badge')).toHaveTextContent('Unknown: no reason given')
  })
})
