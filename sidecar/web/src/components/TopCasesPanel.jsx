import { SeverityBadge } from './SeverityBadge'
import { ErrorBanner } from './ErrorBanner'
import { CasesHealthNotes } from '../pages/cases/CasesHealthNotes'
import { caseLink } from '../pages/cases/casePresentation'
import { TOP_CASES_LIMIT } from '../lib/topCases'

const muted = { color: 'var(--text-secondary)' }

function allCasesHref(database) {
  return database && database !== 'all'
    ? `#/cases?${new URLSearchParams({ database })}` : '#/cases'
}

function TopCaseRow({ row }) {
  return (
    <a href={caseLink('cases', row)} data-testid="top-case-link"
      className="block rounded p-2" style={{ background: 'var(--bg-primary)' }}>
      <div data-testid="top-case-row" className="flex items-center gap-3">
        <SeverityBadge severity={row.severity} />
        <span className="flex-1 min-w-0">
          <span className="block text-sm" style={{ color: 'var(--text-primary)' }}>
            {row.title || row.id}
          </span>
          {row.rank_basis && (
            <span className="block text-xs" style={muted}>{row.rank_basis}</span>
          )}
        </span>
        <span className="text-xs" style={muted}>{row.database_name}</span>
      </div>
    </a>
  )
}

// TopCasesBody says what the query knows: an error, loading, an answer it
// cannot read, the ranked rows, or no open cases only when every database
// answered (cases-05: an unreachable database is named, never blanked).
function TopCasesBody({ query, database }) {
  const { data, error, refetch } = query
  if (error && !data) return <ErrorBanner message={error} onRetry={refetch} />
  if (!data) {
    return <div className="h-16 rounded animate-pulse" data-testid="top-cases-loading"
      style={{ background: 'var(--bg-hover)' }} />
  }
  if (!Array.isArray(data.cases)) {
    return <p className="text-sm" style={muted}>The issue list could not be read.</p>
  }
  const rows = data.cases.slice(0, TOP_CASES_LIMIT)
  const unavailable = Array.isArray(data.unavailable) ? data.unavailable : []
  const total = Number.isInteger(data.total) ? data.total : rows.length
  // A database cut at its read cap has more open cases than total counts.
  const cut = Object.keys(data.truncated || {}).length > 0
  return (
    <div className="space-y-2">
      <CasesHealthNotes unavailable={unavailable} truncated={data.truncated} />
      {rows.length > 0 ? (
        <>
          <p className="text-xs" style={muted}>
            {total > rows.length || cut
              ? `Top ${rows.length} of ${cut ? 'at least ' : ''}${total} open issues`
              : `${rows.length} open ${rows.length === 1 ? 'case' : 'cases'}`}
            , ranked by severity, then time to impact.
          </p>
          {rows.map(row => <TopCaseRow key={`${row.database_name}|${row.id}`} row={row} />)}
        </>
      ) : unavailable.length === 0 && (
        <p className="text-sm" style={muted}>No open issues in the selected scope.</p>
      )}
      <a href={allCasesHref(database)} data-testid="top-cases-all"
        className="inline-block mt-1 text-sm" style={{ color: 'var(--accent)' }}>
        View all issues
      </a>
    </div>
  )
}

// TopCasesPanel replaces the Overview's newest-recommendations list with
// the highest-ranked open cases, each linking to its case (cases-11b).
export function TopCasesPanel({ query, database }) {
  return (
    <div className="rounded p-4" data-testid="top-cases"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      <h2 className="text-sm font-medium mb-3" style={muted}>Top issues</h2>
      <TopCasesBody query={query} database={database} />
    </div>
  )
}
