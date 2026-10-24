// Change Review helpers shared by the Reviews page and its parts: API
// URLs, the #/reviews route, labels and formatting. Unknown values are
// shown as a dash, never as zero.

export const DASH = '—'
export const MODEL_SUGGESTION_LABEL = "suggested by pg_sage's model — verify"

export const cardStyle = {
  background: 'var(--bg-card)',
  border: '1px solid var(--border)',
}

const SEVERITY_ORDER = ['critical', 'warning', 'info']

function oneDatabase(database) {
  return database && database !== 'all' ? database : ''
}

// reviewsListURL lists the fleet's reviews, or one database's.
export function reviewsListURL(database) {
  const db = oneDatabase(database)
  return db ? `/api/v1/change-reviews?database=${encodeURIComponent(db)}`
    : '/api/v1/change-reviews'
}

export function reviewSummaryURL(database) {
  const db = oneDatabase(database)
  return db ? `/api/v1/change-reviews/summary?database=${encodeURIComponent(db)}`
    : '/api/v1/change-reviews/summary'
}

// reviewDetailURL addresses one review; null without a database or id.
export function reviewDetailURL(database, id) {
  const db = oneDatabase(database)
  if (!db || !id) return null
  return `/api/v1/databases/${encodeURIComponent(db)}/change-reviews/` +
    encodeURIComponent(id)
}

export function reviewVerdictURL(database, id) {
  const base = reviewDetailURL(database, id)
  return base ? `${base}/verdict` : null
}

// reviewLink is the UI route of a review.
export function reviewLink(database, id) {
  return `#/reviews?${new URLSearchParams({ id, database })}`
}

// parseReviewRoute reads id and database from a #/reviews?... hash.
export function parseReviewRoute(hash) {
  const query = String(hash || '').split('?')[1] || ''
  const params = new URLSearchParams(query)
  return { id: params.get('id') || '', database: params.get('database') || '' }
}

export function canDecideReviews(user) {
  return user?.role === 'admin' || user?.role === 'operator'
}

// prLabel names a review's PR: title, repo#number, URL, or review id.
export function prLabel(review) {
  if (!review) return 'review'
  if (review.pr_title) return review.pr_title
  if (review.repo && review.pr_number > 0) return `${review.repo}#${review.pr_number}`
  if (review.pr_url) return review.pr_url
  return review.id ? `review ${review.id.slice(0, 8)}` : 'review'
}

const DBA_LABELS = {
  concur: 'Concur',
  concur_with_edits: 'Concur with edits',
  override: 'Override',
}

export function humanize(value) {
  return String(value || '').replaceAll('_', ' ')
}

export function dbaVerdictLabel(review) {
  if (review?.dba_verdict) {
    const label = DBA_LABELS[review.dba_verdict] || humanize(review.dba_verdict)
    return review.dba_direction ? `${label} (${humanize(review.dba_direction)})` : label
  }
  if (review?.state === 'complete' &&
    (review.verdict === 'review' || review.verdict === 'risky')) {
    return 'Awaiting DBA'
  }
  return DASH
}

const RECEIPT_LABELS = {
  clean: 'clean',
  clean_low_traffic: 'clean, low traffic',
  regressed: 'regressed',
  unverifiable: 'unverifiable',
}

// receiptState is where a review is on its way to a receipt.
export function receiptState(review) {
  if (review?.state === 'running') return 'review running'
  if (review?.state === 'failed') return 'review failed'
  switch (review?.landing_state) {
    case 'expired':
      return 'landing expired'
    case 'not_applicable':
      return 'already in place'
    case 'landed':
      return RECEIPT_LABELS[review.receipt_verdict] || 'landed, receipt pending'
    default:
      return 'awaiting landing'
  }
}

// groupFindings groups findings by severity, most severe first.
export function groupFindings(findings) {
  const groups = SEVERITY_ORDER.map(severity => ({
    severity, findings: (findings || []).filter(f => f.severity === severity),
  }))
  return groups.filter(g => g.findings.length > 0)
}

// formatDuration shows seconds as minutes, hours or days.
export function formatDuration(seconds) {
  if (seconds === null || seconds === undefined || Number.isNaN(Number(seconds))) {
    return DASH
  }
  const s = Number(seconds)
  if (s < 3600) return `${Math.round(s / 60)}m`
  if (s < 86400) return `${(s / 3600).toFixed(1)}h`
  return `${(s / 86400).toFixed(1)}d`
}

export function formatPct(rate) {
  if (rate === null || rate === undefined) return DASH
  return `${Math.round(rate * 100)}%`
}

// concurrenceTotal sums concurrence over the verdict classes.
export function concurrenceTotal(concurrence) {
  let n = 0
  let of = 0
  for (const rate of Object.values(concurrence || {})) {
    n += rate?.n || 0
    of += rate?.of || 0
  }
  return { n, of, rate: of > 0 ? n / of : null }
}

export function formatNumber(value) {
  return Number(value || 0).toLocaleString('en-US')
}

export function formatMs(value) {
  if (value === null || value === undefined) return DASH
  return `${Number(value).toFixed(2)} ms`
}

export function formatTime(value) {
  if (!value) return DASH
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return DASH
  return d.toISOString().slice(0, 16).replace('T', ' ') + ' UTC'
}
