import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'
import { parseReviewRoute, reviewSummaryURL, reviewsListURL } from '../lib/reviews'
import { ReviewList } from './reviews/ReviewList'
import { ReviewDetail } from './reviews/ReviewDetail'
import { ReviewMetrics } from './reviews/ReviewMetrics'

// Change Review: pg_sage's pre-merge reviews of migrations and
// data-access changes. The list shows every review of the selected
// database (or the fleet) with its verdict, DBA verdict and receipt, and
// the measured track record; #/reviews?id=&database= opens one review,
// where operators and admins record the DBA verdict. pg_sage never
// approves or merges anything: the verdict is advice, the DBA decides.

const LIST_POLL_MS = 30000

function Intro() {
  return (
    <div>
      <h2 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
        Change reviews
      </h2>
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        pg_sage reviews migrations and data-access changes against the live database
        before they merge, then checks the receipt after they land. Reviews marked
        review or risky wait for a DBA verdict here and in Cases.
      </p>
    </div>
  )
}

function ReviewsList({ database }) {
  const list = useAPI(reviewsListURL(database), LIST_POLL_MS)
  const summary = useAPI(reviewSummaryURL(database), LIST_POLL_MS)
  const reviews = list.data?.reviews || []
  return (
    <div className="space-y-4">
      <Intro />
      <ReviewMetrics summary={summary.data} />
      {summary.error && <ErrorBanner message={summary.error} onRetry={summary.refetch} />}
      {list.data?.partial && (
        <p data-testid="reviews-partial" className="text-xs"
          style={{ color: 'var(--yellow)' }}>
          Partial list: {(list.data.unavailable || []).join(', ')} could not be read.
        </p>
      )}
      {list.loading && !list.data && <LoadingSpinner />}
      {list.error && <ErrorBanner message={list.error} onRetry={list.refetch} />}
      {list.data && reviews.length === 0 && (
        <div data-testid="reviews-empty">
          <EmptyState message={'No change reviews yet. CI or a coding agent requests ' +
            'one with review_change over MCP (pg_sage review submit).'} />
        </div>
      )}
      {reviews.length > 0 && <ReviewList reviews={reviews} />}
    </div>
  )
}

export function ReviewsPage({ database, user }) {
  const [route] = useState(() => parseReviewRoute(window.location.hash))
  if (route.id) {
    const db = route.database || (database !== 'all' ? database : '')
    if (!db) {
      return (
        <p data-testid="review-pick-database" className="text-sm"
          style={{ color: 'var(--text-secondary)' }}>
          Pick the review's database in the database selector to open it.
        </p>
      )
    }
    return (
      <div data-testid="reviews-page">
        <ReviewDetail database={db} id={route.id} user={user} />
      </div>
    )
  }
  return (
    <div data-testid="reviews-page">
      <ReviewsList database={database} />
    </div>
  )
}
