import {
  INBOX_HREF, NO_REVIEWS, countText, gateLine, gateRow, originRows, ratio, reviewsCount,
  verdictLabel,
} from '../../lib/scorecard'
import { reviewHref } from '../../lib/review'
import { formatTime } from '../../lib/time'

const muted = { color: 'var(--text-secondary)' }
const cell = 'px-2 py-1 text-right align-top'

// Scorecard (review-05b) is Sage Review's proof: what each origin proposed
// and what became of it, and the model gate's state. The numbers are
// review-05a's; this only shows them, each with its denominator. compact
// is the Value page's section: the gate card and the improved and
// reverted counts, with a link to the full review.
export function Scorecard({ data, user, compact = false, database }) {
  const scorecard = data?.scorecard
  const reviews = reviewsCount(scorecard)
  const rows = originRows(scorecard?.by_origin)
  return (
    <div className="space-y-3">
      <GateCard gate={data?.gate} thresholds={data?.thresholds}
        admin={user?.role === 'admin'} />
      {reviews === 0 ? <p className="text-sm">{NO_REVIEWS}</p> : (
        <>
          <p data-testid="scorecard-reviews" className="text-xs" style={muted}>
            {reviews === null ? 'Reviews: invalid' : `${reviews} reviews`}
            {scorecard?.since ? ` since ${formatTime(scorecard.since)}` : ''}
          </p>
          {rows.length > 0 && <OriginTable rows={rows} compact={compact} />}
        </>
      )}
      {compact && database && (
        <a href={reviewHref(database)} className="inline-block text-sm underline"
          style={{ color: 'var(--accent)' }}>Open the review</a>
      )}
    </div>
  )
}

// GateCard is the newest G-M1 evaluation: the line of counts, its verdict
// and reason, and for an admin a pending promotion's way to the Inbox,
// where every autonomy promotion is decided. G-M2 is one line below.
function GateCard({ gate, thresholds, admin }) {
  const m1 = gateRow(gate, 'G-M1')
  const m2 = gateRow(gate, 'G-M2')
  const pending = m1?.verdict === 'pending_approval'
  return (
    <div data-testid="gate-card" className="rounded p-3 space-y-1"
      style={{ border: '1px solid var(--border)', overflowWrap: 'anywhere' }}>
      <p data-testid="gate-line" className="text-sm font-medium">{gateLine(m1, thresholds)}</p>
      {m1 && (
        <p data-testid="gate-verdict" className="text-xs">
          {verdictLabel(m1.verdict)}
          {m1.verdict === 'granted' && m1.approved_by ? ` (${m1.approved_by})` : ''}
        </p>
      )}
      {m1?.reason && <p data-testid="gate-reason" className="text-xs" style={muted}>
        {m1.reason}</p>}
      {m2?.reason && <p data-testid="gate-m2" className="text-xs" style={muted}>
        {m2.reason}</p>}
      {pending && admin && (
        <a href={INBOX_HREF} className="inline-block text-sm underline"
          style={{ color: 'var(--accent)' }}>Approve in the Inbox</a>
      )}
    </div>
  )
}

const FULL = ['proposed', 'accepted', 'improved', 'no_change', 'regressed', 'reverted',
  'pending']
const COMPACT = ['proposed', 'improved', 'reverted']
const HEADINGS = Object.freeze({
  proposed: 'Proposed', accepted: 'Accepted', improved: 'Improved', no_change: 'No change',
  regressed: 'Regressed', reverted: 'Reverted', pending: 'Pending',
})

function OriginTable({ rows, compact }) {
  const columns = compact ? COMPACT : FULL
  return (
    <div className="overflow-x-auto">
      <table aria-label="Review scorecard by origin" className="w-full text-sm">
        <thead>
          <tr style={muted}>
            <th scope="col" className="px-2 py-1 text-left">Origin</th>
            {columns.map(c => <th key={c} scope="col" className={cell}>{HEADINGS[c]}</th>)}
          </tr>
        </thead>
        <tbody>
          {rows.map(row => (
            <tr key={row.origin} data-testid={`scorecard-row-${row.origin}`}
              style={{ borderTop: '1px solid var(--border)' }}>
              <th scope="row" className="px-2 py-1 text-left font-medium">{row.label}</th>
              {columns.map(c => <CountCell key={c} column={c} counts={row.counts} />)}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// CountCell is one count: accepted is of proposed, improved and reverted
// are of decided with their rate, the rest are plain counts.
function CountCell({ column, counts }) {
  let body
  if (column === 'accepted') {
    body = ratio(counts.accepted, counts.proposed).text
  } else if (column === 'improved' || column === 'reverted') {
    const r = ratio(column === 'improved' ? counts.verified_improved : counts.reverted,
      counts.decided)
    body = <>{r.text}{r.valid && <span className="block text-xs" style={muted}>{r.rate}</span>}</>
  } else {
    body = countText(counts[column])
  }
  return <td data-testid={`cell-${column}`} className={cell}>{body}</td>
}
