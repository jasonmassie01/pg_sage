import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ReviewPage } from './ReviewPage'
import { gradePayload, gradedRun, templatedReport } from '../test/reviewFixtures'

// review-01b: the Review page reads GET .../review/grade, shows the grade,
// its dimensions, the delta and the report, and sends the read receipt
// (POST .../review/runs/{id}/viewed) once, never blocking the page.
// No concurrent access tests here: two tabs opening one review is
// api.TestReviewViewDB_TwoTabsGiveOneRowPerUser (one row per user).

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))
vi.mock('../hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))

let query
function answer(data, extra = {}) {
  query = { data, loading: false, error: null, errorStatus: null, refetch: vi.fn(),
    updatedAt: 1, ...extra }
}

let posts
function stubFetch(statuses = [200]) {
  posts = []
  vi.stubGlobal('fetch', vi.fn(async (url, init = {}) => {
    posts.push({ url, method: init.method, type: init.headers?.['Content-Type'] })
    const status = statuses[Math.min(posts.length - 1, statuses.length - 1)]
    const body = status === 200 ? { run_id: 12, viewed_at: '2026-11-16T10:00:00Z',
      first: true } : { error: 'the read receipt could not be recorded' }
    return { ok: status === 200, status, statusText: '', json: async () => body,
      text: async () => JSON.stringify(body) }
  }))
}

beforeEach(() => {
  api.useAPI.mockReset()
  api.useAPI.mockImplementation(() => query)
  stubFetch()
})
afterEach(() => vi.unstubAllGlobals())

const viewedURL = '/api/v1/databases/orders/review/runs/12/viewed'

describe('ReviewPage', () => {
  it('shows B overall, eight rows, the two moved checks and the report', async () => {
    answer(gradePayload())
    render(<ReviewPage route="/review?db=orders" database="all" />)
    expect(api.useAPI).toHaveBeenCalledWith('/api/v1/databases/orders/review/grade', 60000)
    expect(screen.getByTestId('grade-card')).toHaveTextContent('B')
    expect(screen.getAllByTestId('dimension-row')).toHaveLength(8)
    expect(screen.getAllByTestId('delta-check')).toHaveLength(2)
    expect(screen.getAllByTestId('report-finding')).toHaveLength(2)
    // The server refuses a POST without Content-Type: application/json (415).
    await waitFor(() => expect(posts).toEqual([{ url: viewedURL, method: 'POST',
      type: 'application/json' }]))
  })

  it('says the first grade runs after the first look when there is none', () => {
    answer(gradePayload({ latest: null, previous: null, delta: null, report: null }))
    render(<ReviewPage route="/review" database="orders" />)
    expect(screen.getByText('First grade runs after the first look.')).toBeInTheDocument()
    expect(screen.queryByTestId('grade-card')).toBeNull()
    expect(posts).toEqual([])
  })

  it('asks for a database instead of reading every one', () => {
    answer(null)
    render(<ReviewPage route="/review" database="all" />)
    expect(screen.getByText('Select a database to see its review.')).toBeInTheDocument()
    expect(api.useAPI).toHaveBeenCalledWith(null, expect.any(Number))
  })

  it('shows the QueryState error with Retry on a 500', () => {
    const refetch = vi.fn()
    answer(null, { error: 'the review grade of orders could not be read (500)',
      errorStatus: 500, refetch })
    render(<ReviewPage route="/review?db=orders" />)
    expect(screen.getByRole('alert'))
      .toHaveTextContent('the review grade of orders could not be read (500)')
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(refetch).toHaveBeenCalledTimes(1)
  })

  it('sends no receipt for a review this person has already opened', async () => {
    answer(gradePayload({ viewed_at: '2026-11-16T09:30:00Z' }))
    render(<ReviewPage route="/review?db=orders" />)
    await new Promise(r => setTimeout(r, 20))
    expect(posts).toEqual([])
  })

  it('sends the receipt once across re-renders and polls', async () => {
    answer(gradePayload())
    const { rerender } = render(<ReviewPage route="/review?db=orders" />)
    await waitFor(() => expect(posts).toHaveLength(1))
    answer(gradePayload())
    rerender(<ReviewPage route="/review?db=orders" />)
    await new Promise(r => setTimeout(r, 20))
    expect(posts).toHaveLength(1)
  })

  it('retries a failed receipt once and never blocks the page', async () => {
    stubFetch([500, 500, 500])
    answer(gradePayload())
    render(<ReviewPage route="/review?db=orders" />)
    await waitFor(() => expect(posts).toHaveLength(2), { timeout: 3000 })
    await new Promise(r => setTimeout(r, 1500))
    expect(posts).toHaveLength(2)
    expect(screen.getByTestId('grade-card')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('swaps the running banner for the new report on the next poll', () => {
    const running = { id: 13, kind: 'grade', started_at: '2026-11-16T09:00:00Z' }
    answer(gradePayload({ running, viewed_at: '2026-11-16T09:30:00Z' }))
    const { rerender } = render(<ReviewPage route="/review?db=orders" />)
    expect(screen.getByTestId('review-running')).toHaveTextContent('A grade is running')
    expect(api.useAPI).toHaveBeenLastCalledWith(
      '/api/v1/databases/orders/review/grade', 5000)
    answer(gradePayload({ latest: gradedRun(13), report: templatedReport({ run_id: 13 }),
      viewed_at: '2026-11-16T09:31:00Z' }))
    rerender(<ReviewPage route="/review?db=orders" />)
    expect(screen.queryByTestId('review-running')).toBeNull()
    expect(screen.getByTestId('review-report')).toHaveAttribute('data-run', '13')
    expect(api.useAPI).toHaveBeenLastCalledWith(
      '/api/v1/databases/orders/review/grade', 60000)
  })

  it('shows the banner alone while the first grade runs', () => {
    answer(gradePayload({ latest: null, previous: null, delta: null, report: null,
      running: { id: 1, kind: 'grade', started_at: '2026-11-16T09:00:00Z' } }))
    render(<ReviewPage route="/review?db=orders" />)
    expect(screen.getByTestId('review-running')).toBeInTheDocument()
    expect(screen.queryByText('First grade runs after the first look.')).toBeNull()
    expect(posts).toEqual([])
  })
})
