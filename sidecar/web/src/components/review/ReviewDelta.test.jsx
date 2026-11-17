import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ReviewDelta } from './ReviewDelta'
import { delta, gradedRun } from '../../test/reviewFixtures'
import { formatTime } from '../../lib/time'

// review-01b: "What changed since <date>", against the previous grade.
// No concurrent access tests: a pure render.

const previous = gradedRun(11, { finished_at: '2026-11-15T09:00:04Z' })
const since = formatTime('2026-11-15T09:00:04Z')

describe('ReviewDelta', () => {
  it('lists the two moved checks since the previous grade', () => {
    render(<ReviewDelta delta={delta()} previous={previous} />)
    const section = screen.getByTestId('review-delta')
    expect(section).toHaveTextContent(`What changed since ${since}`)
    expect(section).toHaveTextContent('Overall C → B')
    const moves = screen.getAllByTestId('delta-check')
    expect(moves.map(m => m.textContent)).toEqual([
      'schema.duplicate_index: Fail → Pass', 'lifecycle.extension_drift: Fail → Warn'])
  })

  it('says no change since the date for an empty delta', () => {
    render(<ReviewDelta delta={delta({ dimensions: [], from_overall: 'B', to_overall: 'B' })}
      previous={previous} />)
    expect(screen.getByTestId('review-delta')).toHaveTextContent(`No change since ${since}`)
    expect(screen.queryAllByTestId('delta-check')).toHaveLength(0)
  })

  it('says a first grade has nothing to compare with', () => {
    render(<ReviewDelta delta={delta({ first_run: true })} previous={null} />)
    expect(screen.getByTestId('review-delta'))
      .toHaveTextContent('First grade: there is no earlier grade to compare with.')
    expect(screen.queryAllByTestId('delta-check')).toHaveLength(0)
  })

  it('names a rubric change, which explains moves that are not the database', () => {
    render(<ReviewDelta delta={delta({ rubric_changed: true })} previous={previous} />)
    expect(screen.getByTestId('review-delta')).toHaveTextContent('The rubric changed')
  })

  it('shows a newly added check, which has no earlier status', () => {
    render(<ReviewDelta previous={previous} delta={delta({ dimensions: [{ dimension: 'schema',
      to_letter: 'B', checks: [{ check: 'schema.new', to: 'fail' }] }] })} />)
    expect(screen.getByTestId('delta-check').textContent).toBe('schema.new: new → Fail')
  })

  it('renders nothing with no delta', () => {
    const { container } = render(<ReviewDelta delta={null} previous={null} />)
    expect(container).toBeEmptyDOMElement()
  })
})
