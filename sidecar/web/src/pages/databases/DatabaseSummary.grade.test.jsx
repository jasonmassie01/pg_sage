import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabaseSummary } from './DatabaseSummary'
import { gradePayload } from '../../test/reviewFixtures'

// review-01b: the database page header shows the latest grade chip, which
// links to the Review page. No concurrent access tests: one mocked render.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))
vi.mock('../../hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))

describe('DatabaseSummary grade chip', () => {
  beforeEach(() => {
    api.useAPI.mockReset()
    api.useAPI.mockImplementation(url => ({ loading: false, error: null, refetch: vi.fn(),
      data: String(url).includes('/review/grade') ? gradePayload()
        : { databases: [], lookalikes: [] } }))
  })

  it('shows the latest grade in the header, linking to /review?db=', () => {
    render(<DatabaseSummary database="orders" />)
    const header = screen.getByRole('heading', { name: /Database: orders/ })
    const chip = screen.getByTestId('grade-chip')
    expect(header.parentElement).toContainElement(chip)
    expect(chip).toHaveTextContent('Grade: B')
    expect(chip).toHaveAttribute('href', '#/review?db=orders')
  })
})
