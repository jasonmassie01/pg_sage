import { TimeAgo } from '../../components/TimeAgo'
import {
  cardStyle, dbaVerdictLabel, prLabel, receiptState, reviewLink,
} from '../../lib/reviews'

// ReviewList is the Reviews table: one row per review with its PR,
// database, verdict, DBA verdict, receipt state and age.

const VERDICT_COLORS = {
  safe: 'var(--green)',
  review: 'var(--yellow)',
  risky: 'var(--red)',
}

export function VerdictBadge({ verdict, testId = 'review-verdict' }) {
  const color = VERDICT_COLORS[verdict] || 'var(--text-secondary)'
  return (
    <span data-testid={testId} className="px-2 py-0.5 rounded text-xs font-medium"
      style={{ color, border: `1px solid ${color}` }}>
      {verdict || 'pending'}
    </span>
  )
}

function PRCell({ review }) {
  const label = prLabel(review)
  const repoRef = review.repo && review.pr_number > 0
    ? `${review.repo}#${review.pr_number}` : ''
  return (
    <td className="p-2">
      <a href={reviewLink(review.database, review.id)}
        className="text-sm font-medium underline"
        style={{ color: 'var(--text-primary)' }}>
        {label}
      </a>
      {repoRef && repoRef !== label && (
        <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          {repoRef}
        </div>
      )}
    </td>
  )
}

function ReviewRow({ review }) {
  const muted = { color: 'var(--text-secondary)' }
  return (
    <tr data-testid="review-row" style={{ borderTop: '1px solid var(--border)' }}>
      <PRCell review={review} />
      <td data-testid="review-database" className="p-2 text-sm" style={muted}>
        {review.database}
      </td>
      <td className="p-2"><VerdictBadge verdict={review.verdict} /></td>
      <td data-testid="review-dba-verdict" className="p-2 text-sm"
        style={{ color: 'var(--text-primary)' }}>
        {dbaVerdictLabel(review)}
      </td>
      <td data-testid="review-receipt" className="p-2 text-sm" style={muted}>
        {receiptState(review)}
      </td>
      <td data-testid="review-age" className="p-2 text-xs">
        <TimeAgo timestamp={review.created_at} />
      </td>
    </tr>
  )
}

export function ReviewList({ reviews }) {
  return (
    <div className="rounded overflow-x-auto" style={cardStyle}>
      <table className="w-full text-left" aria-label="Change reviews">
        <thead>
          <tr className="text-xs" style={{ color: 'var(--text-secondary)' }}>
            <th className="p-2">Pull request</th><th className="p-2">Database</th>
            <th className="p-2">Verdict</th><th className="p-2">DBA verdict</th>
            <th className="p-2">Receipt</th><th className="p-2">Age</th>
          </tr>
        </thead>
        <tbody>
          {reviews.map(r => <ReviewRow key={`${r.database}/${r.id}`} review={r} />)}
        </tbody>
      </table>
    </div>
  )
}
