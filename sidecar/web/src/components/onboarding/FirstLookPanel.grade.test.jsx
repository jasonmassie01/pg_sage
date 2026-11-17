import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { FirstLookPanel } from './FirstLookPanel'
import { gradePayload, gradedRun } from '../../test/reviewFixtures'

// review-01b: the first look shows the day-one grade chip once review-01a's
// first grade exists. No concurrent access tests: one mocked render.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))
vi.mock('../../hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))

const entry = { database: 'app', report: { database: 'app', duration_ms: 5, relations: 3,
  items: [], checks: [] } }

function respond(grade) {
  api.useAPI.mockImplementation(url => {
    if (url === null) return { data: null, loading: false, error: null, refetch: vi.fn() }
    if (url.includes('/review/grade')) {
      return { data: grade, loading: false, error: null, refetch: vi.fn() }
    }
    return { data: { databases: [entry] }, loading: false, error: null, refetch: vi.fn() }
  })
}

describe('FirstLookPanel grade chip', () => {
  beforeEach(() => api.useAPI.mockReset())

  it('shows the day-one grade of each database, linking to its review', () => {
    respond(gradePayload({ latest: gradedRun(1, { triggers: ['first_look'] }),
      previous: null }))
    render(<FirstLookPanel database="all" />)
    const chip = screen.getByTestId('grade-chip')
    expect(chip).toHaveTextContent('Day-one grade: B')
    expect(chip).toHaveAttribute('href', '#/review?db=app')
    expect(api.useAPI).toHaveBeenCalledWith('/api/v1/databases/app/review/grade',
      expect.any(Number))
  })

  it('shows no chip before the first grade exists', () => {
    respond(gradePayload({ latest: null, previous: null, delta: null, report: null }))
    render(<FirstLookPanel database="all" />)
    expect(screen.queryByTestId('grade-chip')).toBeNull()
    expect(screen.getByText('No problems found in the catalog.')).toBeInTheDocument()
  })
})
