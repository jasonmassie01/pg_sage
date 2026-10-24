import { DASH, cardStyle } from '../../lib/reviews'

// StatementTable lists the statements a review analyzed: where each one
// is (file:line), where it came from (the request's SQL, or translated or
// extracted by the model and then parsed, planned and pinned to an added
// diff line), and the workload query it matches. An extracted statement
// that matches no workload query is a new shape.

const ORIGIN_LABELS = {
  sql_input: 'SQL input',
  model_translated: 'model translated',
  model_extracted: 'model extracted',
}

function location(s) {
  if (!s.file) return DASH
  return s.line ? `${s.file}:${s.line}` : s.file
}

function matched(s) {
  if (s.matched_queryid !== undefined && s.matched_queryid !== null) {
    return String(s.matched_queryid)
  }
  return s.origin === 'model_extracted' ? 'new shape' : DASH
}

function confidence(s) {
  if (s.origin === 'sql_input' || !s.confidence) return ''
  return `${Math.round(s.confidence * 100)}%`
}

function StatementRow({ s }) {
  const muted = { color: 'var(--text-secondary)' }
  return (
    <tr data-testid="statement-row" style={{ borderTop: '1px solid var(--border)' }}>
      <td className="p-2 text-xs" style={muted}>{s.index}</td>
      <td data-testid="statement-location" className="p-2 text-xs font-mono">
        {location(s)}
      </td>
      <td data-testid="statement-origin" className="p-2 text-xs" style={muted}>
        {ORIGIN_LABELS[s.origin] || s.origin}
        {s.orm ? ` · ${s.orm}` : ''}
      </td>
      <td className="p-2 text-xs" style={muted}>{s.kind}</td>
      <td className="p-2">
        <code className="text-xs whitespace-pre-wrap"
          style={{ color: 'var(--text-primary)' }}>
          {s.sql}
        </code>
      </td>
      <td data-testid="statement-queryid" className="p-2 text-xs font-mono">
        {matched(s)}
      </td>
      <td className="p-2 text-xs" style={muted}>{confidence(s)}</td>
    </tr>
  )
}

export function StatementTable({ statements }) {
  if (!statements?.length) {
    return (
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        No statements.
      </p>
    )
  }
  return (
    <div className="rounded overflow-x-auto" style={cardStyle}>
      <table className="w-full text-left" aria-label="Statements">
        <thead>
          <tr className="text-xs" style={{ color: 'var(--text-secondary)' }}>
            <th className="p-2">#</th><th className="p-2">Location</th>
            <th className="p-2">Origin</th><th className="p-2">Kind</th>
            <th className="p-2">SQL</th><th className="p-2">Matched queryid</th>
            <th className="p-2">Confidence</th>
          </tr>
        </thead>
        <tbody>
          {statements.map(s => <StatementRow key={s.index} s={s} />)}
        </tbody>
      </table>
    </div>
  )
}
