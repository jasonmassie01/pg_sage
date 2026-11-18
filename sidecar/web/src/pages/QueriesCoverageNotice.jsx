import { useAPI } from '../hooks/useAPI'

// CoverageNotice (evid-01b): one line under the top statements heading on
// how much of the execution time the long per-query history covers. It
// speaks only when there is news (delta-only): below 90% covered, while
// learning, when statistics reset often (a compute that suspends), or
// when the coverage is unknown; silent when ok above 90% and when the
// coverage cannot be read (the notice is advisory, the table is not).

const SILENT_ABOVE = 0.9

function plural(n, word) {
  return `${n} ${word}${n === 1 ? '' : 's'}`
}

// noticeText is the entry's line, or null when there is nothing to say.
function noticeText(entry) {
  if (entry.status === 'mostly_breaks') {
    return 'Statistics reset often here (compute suspends?): '
      + 'long-window figures are mostly breaks.'
  }
  const share = entry.covered_exec_share
  if (entry.status === 'unknown' || share == null) {
    return 'Long history: coverage unknown (no statement totals recorded for this window).'
  }
  if (entry.status === 'ok' && share > SILENT_ABOVE) return null
  const days = Math.max(1, Math.round((entry.hours || 0) / 24))
  const line = `Long history: ${Math.round(share * 100)}% of execution time covered, `
    + `${plural(entry.epoch_breaks || 0, 'epoch break')} in ${plural(days, 'day')}`
  return entry.status === 'learning' ? `${line} (still learning: under 14 days)` : line
}

export function CoverageNotice({ database }) {
  const coverage = useAPI(
    `/api/v1/queries/coverage?database=${encodeURIComponent(database)}`, 300000)
  const entries = coverage.error ? [] : (coverage.data?.databases || [])
  const fleet = database === 'all'
  const lines = entries
    .map(e => ({ db: e.database, text: noticeText(e) }))
    .filter(l => l.text)
  if (!lines.length) return null
  return (
    <div className="mb-2">
      {lines.map(l => (
        <p key={l.db} data-testid="coverage-notice" className="text-xs"
          style={{ color: 'var(--text-secondary)' }}>
          {fleet ? `${l.db}: ${l.text}` : l.text}
        </p>
      ))}
    </div>
  )
}
