import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ValuePage } from './ValuePage'
import { ReviewPage } from './ReviewPage'
import { gradePayload } from '../test/reviewFixtures'
import golden from '../../../internal/api/testdata/review_scorecard.golden.json'
  with { type: 'json' }

// review-05b: the scorecard is a compact section of the Value page and a
// section of the Review page. Each reads its own route, so a scorecard
// that fails leaves the rest of the page standing.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))
vi.mock('../hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))

const SCORECARD = '/api/v1/databases/orders/review/scorecard'
const value = { dba_hours_saved: { all_time: 12, this_month: 3, this_week: 1 },
  by_feature: { index: 12 }, by_database: [{ name: 'orders', hours: 12 }],
  incidents_avoided: { count: 0, credited_hours: 0, detail: [] },
  potential_hours_pending: 2, trend_daily: [] }

function ok(data) {
  return { data, loading: false, error: null, refetch: vi.fn(), updatedAt: 1 }
}

function failed(error) {
  return { data: null, loading: false, error, errorStatus: 500, refetch: vi.fn() }
}

let replies
beforeEach(() => {
  replies = {}
  api.useAPI.mockReset()
  api.useAPI.mockImplementation(url => replies[url] ?? ok(null))
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, status: 200,
    json: async () => ({}), text: async () => '{}' })))
})

describe('Value page scorecard', () => {
  it('shows the compact scorecard of the selected database', () => {
    replies['/api/v1/value?database=orders'] = ok(value)
    replies[SCORECARD] = ok(structuredClone(golden))
    render(<ValuePage database="orders" user={{ role: 'viewer' }} />)
    const section = screen.getByTestId('review-scorecard')
    expect(within(section).getByTestId('gate-line'))
      .toHaveTextContent('Model gate: 50 of 50 reviews; model 90% improved')
    expect(within(section).getByRole('link', { name: 'Open the review' }))
      .toHaveAttribute('href', '#/review?db=orders')
    expect(screen.getByText('12 h', { selector: 'div' })).toBeInTheDocument()
  })

  it('a scorecard 500 shows its error and the rest of the page still renders', () => {
    replies['/api/v1/value?database=orders'] = ok(value)
    replies[SCORECARD] = failed('the review scorecard of orders could not be read')
    render(<ValuePage database="orders" user={{ role: 'viewer' }} />)
    const section = screen.getByTestId('review-scorecard')
    expect(within(section).getByRole('alert'))
      .toHaveTextContent('the review scorecard of orders could not be read')
    expect(screen.getByRole('region', { name: 'Realized DBA hours saved' }))
      .toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Value delivered' })).toBeInTheDocument()
  })

  it('the whole fleet asks for a database instead of reading a scorecard', () => {
    replies['/api/v1/value'] = ok(value)
    render(<ValuePage database="all" user={{ role: 'admin' }} />)
    expect(api.useAPI).not.toHaveBeenCalledWith(expect.stringContaining('/scorecard'),
      expect.anything())
    expect(screen.getByText('Select a database to see its review scorecard.'))
      .toBeInTheDocument()
  })
})

describe('Review page scorecard', () => {
  it('shows the full scorecard under the report', () => {
    replies['/api/v1/databases/orders/review/grade'] = ok(gradePayload())
    replies[SCORECARD] = ok(structuredClone(golden))
    render(<ReviewPage route="/review?db=orders" database="all" user={{ role: 'admin' }} />)
    const section = screen.getByTestId('review-scorecard')
    expect(within(section).getByTestId('scorecard-row-model')).toBeInTheDocument()
    expect(within(section).getAllByTestId('cell-accepted')).toHaveLength(2)
    expect(within(section).getByRole('link', { name: 'Approve in the Inbox' }))
      .toHaveAttribute('href', '#/')
    expect(screen.getByTestId('review-report')).toBeInTheDocument()
  })

  it('a scorecard 500 leaves the grade and the report standing', () => {
    replies['/api/v1/databases/orders/review/grade'] = ok(gradePayload())
    replies[SCORECARD] = failed('the review scorecard of orders could not be read')
    render(<ReviewPage route="/review?db=orders" database="all" user={{ role: 'admin' }} />)
    expect(within(screen.getByTestId('review-scorecard')).getByRole('alert'))
      .toHaveTextContent('could not be read')
    expect(screen.getByTestId('grade-card')).toBeInTheDocument()
    expect(screen.getAllByTestId('report-finding')).toHaveLength(2)
  })
})
