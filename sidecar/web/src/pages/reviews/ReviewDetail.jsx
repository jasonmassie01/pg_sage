import { useEffect } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { ErrorBanner } from '../../components/ErrorBanner'
import {
  cardStyle, formatNumber, humanize, prLabel, reviewDetailURL,
} from '../../lib/reviews'
import { VerdictBadge } from './ReviewList'
import { FindingList } from './FindingList'
import { StatementTable } from './StatementTable'
import { VerdictForm } from './VerdictForm'
import { ReceiptPanel } from './ReceiptPanel'

// ReviewDetail is one review: its verdict and state, the model's status
// and tokens with the count of model claims the gates dropped, findings
// by severity with their evidence, the statements, what could not be
// determined, the DBA verdict, the receipt and the credit. A running
// review is polled until it completes.

const RUNNING_POLL_MS = 5000
const muted = { color: 'var(--text-secondary)' }

function Section({ title, children }) {
  return (
    <section className="space-y-2" aria-label={title}>
      <h3 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
        {title}
      </h3>
      {children}
    </section>
  )
}

function stateText(review) {
  if (review.state === 'failed') return `failed: ${review.failure_reason || 'unknown'}`
  if (review.state === 'running') return `running (${humanize(review.stage) || 'queued'})`
  return 'complete'
}

function ModelLine({ review }) {
  const dropped = review.summary?.dropped_count ?? (review.dropped || []).length
  const appCode = review.summary?.app_code_analyzed
  return (
    <div className="flex flex-wrap gap-3 text-xs" style={muted}>
      <span data-testid="review-model-status">
        Model: {humanize(review.model_status) || 'pending'} ·
        {' '}{formatNumber(review.tokens)} tokens
      </span>
      {dropped > 0 && (
        <span data-testid="review-dropped">
          {dropped} model claim{dropped === 1 ? '' : 's'} dropped by the evidence gates
        </span>
      )}
      {review.state === 'complete' && appCode === false && (
        <span data-testid="review-app-code">App-code changes were not analyzed.</span>
      )}
      <span>Parse mode: {review.parse_mode || '—'}</span>
      {review.deterministic_verdict && (
        <span>Deterministic verdict: {review.deterministic_verdict}</span>
      )}
    </div>
  )
}

function Header({ review }) {
  return (
    <div className="rounded p-3 space-y-2" style={cardStyle}>
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="text-base font-semibold" style={{ color: 'var(--text-primary)' }}>
          {prLabel(review)}
        </h2>
        <VerdictBadge verdict={review.verdict} testId="review-detail-verdict" />
        <span data-testid="review-state" className="text-xs" style={muted}>
          {stateText(review)}
        </span>
      </div>
      <div className="flex flex-wrap gap-3 text-xs" style={muted}>
        <span>Database: {review.database}</span>
        {review.pr_url && (
          <a href={review.pr_url} target="_blank" rel="noreferrer noopener"
            className="underline" style={{ color: 'var(--accent)' }}>
            {review.pr_url}
          </a>
        )}
        {review.head_sha && <span className="font-mono">{review.head_sha}</span>}
        <span>Requested by {review.actor} via {review.source}</span>
      </div>
      <ModelLine review={review} />
    </div>
  )
}

function NotDetermined({ items }) {
  if (!items?.length) return null
  return (
    <Section title="Not determined">
      <ul data-testid="review-not-determined" className="text-sm" style={muted}>
        {items.map((n, i) => <li key={i}>{n.subject}: {n.reason}</li>)}
      </ul>
    </Section>
  )
}

function Credit({ review }) {
  return (
    <p data-testid="review-credit" className="text-xs" style={muted}>
      Credit: {review.credit_state || 'none'}
      {review.credited_minutes > 0 ? ` (${review.credited_minutes} min)` : ''}
    </p>
  )
}

// PollWhileRunning refetches a running review until it completes.
function PollWhileRunning({ refetch }) {
  useEffect(() => {
    const timer = setInterval(refetch, RUNNING_POLL_MS)
    return () => clearInterval(timer)
  }, [refetch])
  return null
}

export function ReviewDetail({ database, id, user }) {
  const { data, loading, error, refetch } = useAPI(reviewDetailURL(database, id), 0)
  if (loading && !data) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />
  if (!data) return null
  const review = data
  return (
    <div data-testid="review-detail" className="space-y-4">
      {review.state === 'running' && <PollWhileRunning refetch={refetch} />}
      <a href="#/reviews" className="text-xs underline" style={{ color: 'var(--accent)' }}>
        ← All reviews
      </a>
      <Header review={review} />
      <Section title="DBA verdict">
        <VerdictForm review={review} user={user} onDecided={() => refetch()} />
        <Credit review={review} />
      </Section>
      <Section title="Findings">
        <FindingList findings={review.findings} evidence={review.evidence} />
      </Section>
      <Section title="Statements">
        <StatementTable statements={review.statements} />
      </Section>
      <NotDetermined items={review.not_determined} />
      <ReceiptPanel review={review} />
    </div>
  )
}
