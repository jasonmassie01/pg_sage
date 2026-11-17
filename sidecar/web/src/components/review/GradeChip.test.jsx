import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { GradeChip } from './GradeChip'
import { gradePayload, gradedRun } from '../../test/reviewFixtures'

// review-01b: the grade chip on the first look and the database page
// header, linking to the Review page. No concurrent access tests: one
// render reading one mocked query.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))
vi.mock('../../hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))

function answer(data, extra = {}) {
  api.useAPI.mockReturnValue({ data, loading: false, error: null, refetch: vi.fn(),
    ...extra })
}

describe('GradeChip', () => {
  beforeEach(() => api.useAPI.mockReset())

  it('shows the latest letter and links to the Review page of the database', () => {
    answer(gradePayload())
    render(<GradeChip database="orders db" />)
    const chip = screen.getByRole('link', { name: /Grade B/ })
    expect(chip).toHaveAttribute('href', '#/review?db=orders%20db')
    expect(api.useAPI).toHaveBeenCalledWith('/api/v1/databases/orders%20db/review/grade',
      expect.any(Number))
  })

  it('calls the first grade the day-one grade', () => {
    const first = gradedRun(1, { triggers: ['first_look'] })
    answer(gradePayload({ latest: first, previous: null }))
    render(<GradeChip database="orders" firstLook />)
    expect(screen.getByTestId('grade-chip')).toHaveTextContent('Day-one grade')
  })

  it('shows unknown in words when the overall has no letter', () => {
    const run = gradedRun()
    run.grade = { ...run.grade, overall: null, known_dimensions: 2 }
    answer(gradePayload({ latest: run }))
    render(<GradeChip database="orders" />)
    expect(screen.getByTestId('grade-chip')).toHaveTextContent('Unknown')
  })

  it('renders nothing before the first grade, while loading, or on an error', () => {
    for (const q of [{ data: gradePayload({ latest: null }) }, { data: null, loading: true },
      { data: null, error: 'boom' }]) {
      answer(q.data, q)
      const { container, unmount } = render(<GradeChip database="orders" />)
      expect(container).toBeEmptyDOMElement()
      unmount()
    }
  })

  it('asks for nothing with no single database', () => {
    answer(null)
    const { container } = render(<GradeChip database="all" />)
    expect(container).toBeEmptyDOMElement()
    expect(api.useAPI).toHaveBeenCalledWith(null, expect.any(Number))
  })
})
