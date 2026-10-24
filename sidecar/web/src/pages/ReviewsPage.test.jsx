import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ReviewsPage } from './ReviewsPage'
import {
  NOW, admin, listFixture, makeReview, summaryFixture, viewer,
} from './reviews/testkit'

// CHECK-UI-01: the Reviews page lists reviews (PR, database, verdict, DBA
// verdict, receipt state, age) and the detail view shows findings grouped
// by severity with rule ids, citations that expand their evidence,
// statements with file:line, origin and matched queryid, the dropped-claim
// count, and the model status with tokens.

const api = vi.hoisted(() => ({ useAPI: vi.fn(), byURL: {} }))

vi.mock('../hooks/useAPI', () => ({
  useAPI: (...args) => api.useAPI(...args),
}))

function serve(byURL) {
  api.useAPI.mockImplementation(url => {
    const hit = url ? byURL[url] : undefined
    if (hit instanceof Error) {
      return { data: null, loading: false, error: hit.message, refetch: vi.fn() }
    }
    return { data: hit ?? null, loading: hit === undefined && Boolean(url),
      error: null, refetch: vi.fn() }
  })
}

const LIST_ALL = '/api/v1/change-reviews'
const SUMMARY_ALL = '/api/v1/change-reviews/summary'
const DETAIL = '/api/v1/databases/orders/change-reviews/' +
  '11111111-1111-4111-8111-111111111111'

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(NOW)
  api.useAPI.mockReset()
  window.location.hash = '#/reviews'
})

afterEach(() => {
  vi.useRealTimers()
  window.location.hash = '#/'
})

describe('ReviewsPage list', () => {
  it('lists every review with PR, database, verdict, DBA verdict, receipt and age', () => {
    serve({ [LIST_ALL]: listFixture(), [SUMMARY_ALL]: summaryFixture() })
    render(<ReviewsPage database="all" user={viewer} />)
    const rows = screen.getAllByTestId('review-row')
    expect(rows).toHaveLength(3)

    const [risky, concurred, safe] = rows
    expect(risky).toHaveTextContent('Rename orders.email')
    expect(risky).toHaveTextContent('acme/billing#42')
    expect(within(risky).getByTestId('review-database')).toHaveTextContent('orders')
    expect(within(risky).getByTestId('review-verdict')).toHaveTextContent('risky')
    expect(within(risky).getByTestId('review-dba-verdict'))
      .toHaveTextContent('Awaiting DBA')
    expect(within(risky).getByTestId('review-receipt'))
      .toHaveTextContent('awaiting landing')
    expect(within(risky).getByTestId('review-age')).toHaveTextContent('5h ago')

    expect(within(concurred).getByTestId('review-database')).toHaveTextContent('billing')
    expect(within(concurred).getByTestId('review-verdict')).toHaveTextContent('review')
    expect(within(concurred).getByTestId('review-dba-verdict')).toHaveTextContent('Concur')
    expect(within(concurred).getByTestId('review-receipt'))
      .toHaveTextContent('landed, receipt pending')
    expect(within(concurred).getByTestId('review-age')).toHaveTextContent('1d ago')

    // No title or repo: the PR URL names it.
    expect(safe).toHaveTextContent('https://github.com/acme/web/pull/9')
    expect(within(safe).getByTestId('review-verdict')).toHaveTextContent('safe')
    expect(within(safe).getByTestId('review-dba-verdict')).toHaveTextContent('—')
    expect(within(safe).getByTestId('review-receipt')).toHaveTextContent('clean')
    expect(within(safe).getByTestId('review-age')).toHaveTextContent('3d ago')
  })

  it('links each row to its detail view with its own database', () => {
    serve({ [LIST_ALL]: listFixture(), [SUMMARY_ALL]: summaryFixture() })
    render(<ReviewsPage database="all" user={viewer} />)
    const link = within(screen.getAllByTestId('review-row')[1]).getByRole('link',
      { name: /Add index on invoices/ })
    expect(link).toHaveAttribute('href',
      '#/reviews?id=22222222-2222-4222-8222-222222222222&database=billing')
  })

  it('reads one database when one is selected', () => {
    const list = '/api/v1/change-reviews?database=orders'
    const summary = '/api/v1/change-reviews/summary?database=orders'
    serve({ [list]: { reviews: [makeReview()], databases: ['orders'], unavailable: [],
      partial: false }, [summary]: summaryFixture() })
    render(<ReviewsPage database="orders" user={viewer} />)
    expect(screen.getAllByTestId('review-row')).toHaveLength(1)
    const urls = api.useAPI.mock.calls.map(([url]) => url)
    expect(urls).toEqual(expect.arrayContaining([list, summary]))
    expect(urls).not.toContain(LIST_ALL)
  })

  it('says so when there are no reviews yet', () => {
    serve({ [LIST_ALL]: { reviews: [], databases: [], unavailable: [], partial: false },
      [SUMMARY_ALL]: summaryFixture({ reviews: 0 }) })
    render(<ReviewsPage database="all" user={viewer} />)
    expect(screen.queryAllByTestId('review-row')).toHaveLength(0)
    expect(screen.getByTestId('reviews-empty')).toHaveTextContent(/no change reviews/i)
  })

  it('names databases that could not be read', () => {
    serve({ [LIST_ALL]: { ...listFixture(), unavailable: ['billing'], partial: true },
      [SUMMARY_ALL]: summaryFixture() })
    render(<ReviewsPage database="all" user={viewer} />)
    expect(screen.getByTestId('reviews-partial')).toHaveTextContent('billing')
  })

  it('shows a list error with retry', () => {
    serve({ [LIST_ALL]: new Error('503 Service Unavailable'),
      [SUMMARY_ALL]: summaryFixture() })
    render(<ReviewsPage database="all" user={viewer} />)
    expect(screen.getByText(/503 Service Unavailable/)).toBeInTheDocument()
  })
})

describe('ReviewsPage detail', () => {
  function openDetail(review = makeReview(), user = viewer) {
    window.location.hash = '#/reviews?id=11111111-1111-4111-8111-111111111111' +
      '&database=orders'
    serve({ [DETAIL]: review })
    render(<ReviewsPage database="all" user={user} />)
  }

  it('fetches the review of the database named in the link', () => {
    openDetail()
    expect(api.useAPI.mock.calls.map(([url]) => url)).toContain(DETAIL)
    expect(screen.getByTestId('review-detail')).toHaveTextContent('Rename orders.email')
    expect(screen.getByRole('link', { name: /all reviews/i }))
      .toHaveAttribute('href', '#/reviews')
  })

  it('falls back to the selected database when the link names none', () => {
    window.location.hash = '#/reviews?id=11111111-1111-4111-8111-111111111111'
    serve({ [DETAIL]: makeReview() })
    render(<ReviewsPage database="orders" user={viewer} />)
    expect(screen.getByTestId('review-detail')).toBeInTheDocument()
  })

  it('asks to pick a database when neither the link nor the selection names one', () => {
    window.location.hash = '#/reviews?id=11111111-1111-4111-8111-111111111111'
    serve({})
    render(<ReviewsPage database="all" user={viewer} />)
    expect(screen.getByTestId('review-pick-database')).toBeInTheDocument()
    expect(api.useAPI.mock.calls.every(([url]) => !url || !url.includes('/databases/')))
      .toBe(true)
  })

  it('groups findings by severity, most severe first, with rule ids', () => {
    openDetail()
    const groups = screen.getAllByTestId(/^finding-group-/)
    expect(groups.map(g => g.dataset.testid)).toEqual([
      'finding-group-critical', 'finding-group-warning', 'finding-group-info'])
    const critical = screen.getByTestId('finding-group-critical')
    expect(critical).toHaveTextContent('Critical (1)')
    expect(within(critical).getByTestId('finding-rule')).toHaveTextContent('CR-03')
    expect(critical).toHaveTextContent('email is still read by 1 statement')
    const warning = screen.getByTestId('finding-group-warning')
    expect(warning).toHaveTextContent('Warning (2)')
    const rules = within(warning).getAllByTestId('finding-rule').map(e => e.textContent)
    expect(rules).toEqual(['CR-01', 'model'])
    expect(screen.getByTestId('finding-group-info')).toHaveTextContent('CR-15')
  })

  it('labels model suggestions and fences their code', () => {
    openDetail()
    const suggestion = screen.getByTestId('finding-suggestion')
    expect(suggestion).toHaveTextContent("suggested by pg_sage's model — verify")
    expect(within(suggestion).getByText('add_column :orders, :email_address, :text')
      .tagName).toBe('CODE')
  })

  it('expands a citation into its evidence and collapses it again', () => {
    openDetail()
    const critical = screen.getByTestId('finding-group-critical')
    const cite = within(critical).getByRole('button', { name: 'queryid:-4242' })
    expect(screen.queryByTestId('evidence-panel')).toBeNull()
    fireEvent.click(cite)
    const panel = screen.getByTestId('evidence-panel')
    expect(panel).toHaveTextContent('SELECT email FROM orders WHERE id = $1')
    expect(panel).toHaveTextContent('412000')
    expect(panel).toHaveTextContent('aaaaaaaaaaaa')
    expect(cite).toHaveAttribute('aria-expanded', 'true')
    fireEvent.click(cite)
    expect(screen.queryByTestId('evidence-panel')).toBeNull()
  })

  it('says when a cited evidence id is not in the review', () => {
    const review = makeReview()
    review.findings = [{ ...review.findings[1], evidence_ids: ['queryid:999'] }]
    openDetail(review)
    fireEvent.click(screen.getByRole('button', { name: 'queryid:999' }))
    expect(screen.getByTestId('evidence-panel')).toHaveTextContent(/not in this review/i)
  })

  it('shows statements with file:line, origin and matched queryid', () => {
    openDetail()
    const rows = screen.getAllByTestId('statement-row')
    expect(rows).toHaveLength(4)
    expect(within(rows[0]).getByTestId('statement-location'))
      .toHaveTextContent('db/migrate/1_rename.sql:3')
    expect(within(rows[0]).getByTestId('statement-origin')).toHaveTextContent('SQL input')
    expect(within(rows[0]).getByTestId('statement-queryid')).toHaveTextContent('—')
    expect(within(rows[1]).getByTestId('statement-origin'))
      .toHaveTextContent('model extracted')
    expect(within(rows[1]).getByTestId('statement-queryid')).toHaveTextContent('-4242')
    expect(within(rows[2]).getByTestId('statement-origin'))
      .toHaveTextContent('model translated')
    // An extracted statement that matches no workload query is a new shape.
    expect(within(rows[3]).getByTestId('statement-queryid')).toHaveTextContent('new shape')
    expect(rows[1]).toHaveTextContent('80%')
  })

  it('shows the dropped-claim count and the model status with tokens', () => {
    openDetail()
    expect(screen.getByTestId('review-dropped')).toHaveTextContent('2 model claims dropped')
    expect(screen.getByTestId('review-model-status')).toHaveTextContent('ok')
    expect(screen.getByTestId('review-model-status')).toHaveTextContent('18,234 tokens')
  })

  it('says when the model did not run and app code was not analyzed', () => {
    openDetail(makeReview({ model_status: 'no_model', tokens: 0, dropped: [],
      summary: { ...makeReview().summary, dropped_count: 0, app_code_analyzed: false } }))
    expect(screen.getByTestId('review-model-status')).toHaveTextContent('no model')
    expect(screen.getByTestId('review-model-status')).toHaveTextContent('0 tokens')
    expect(screen.queryByTestId('review-dropped')).toBeNull()
    expect(screen.getByTestId('review-app-code')).toHaveTextContent(/not analyzed/i)
  })

  it('lists what could not be determined', () => {
    openDetail()
    expect(screen.getByTestId('review-not-determined'))
      .toHaveTextContent('app/jobs/sync.rb: cut from the offer')
  })

  it('shows a running review as running, without verdict controls', () => {
    openDetail(makeReview({ state: 'running', stage: 'deterministic', verdict: '',
      findings: [], statements: [] }), admin)
    expect(screen.getByTestId('review-state')).toHaveTextContent(/running/i)
    expect(screen.queryByTestId('verdict-concur')).toBeNull()
  })

  it('shows a failed review with its reason', () => {
    openDetail(makeReview({ state: 'failed', failure_reason: 'interrupted', verdict: '' }))
    expect(screen.getByTestId('review-state')).toHaveTextContent('failed: interrupted')
  })

  it('shows the credit state', () => {
    openDetail(makeReview({ dba_verdict: 'concur', dba_actor: 'user:1',
      credit_state: 'pending', credited_minutes: 15 }))
    expect(screen.getByTestId('review-credit')).toHaveTextContent('pending')
    expect(screen.getByTestId('review-credit')).toHaveTextContent('15 min')
  })

  it('shows a detail error', () => {
    window.location.hash = '#/reviews?id=11111111-1111-4111-8111-111111111111' +
      '&database=orders'
    serve({ [DETAIL]: new Error('404 Not Found') })
    render(<ReviewsPage database="all" user={viewer} />)
    expect(screen.getByText(/404 Not Found/)).toBeInTheDocument()
  })
})
