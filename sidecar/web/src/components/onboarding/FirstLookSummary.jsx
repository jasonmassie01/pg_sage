import { SUMMARY_LIST } from '../../lib/fleetScope'

// FirstLookSummary is the first look of a whole fleet (ux-10b): how many
// databases are at each grade and the worst SUMMARY_LIST of those that
// need attention, each opening its own report through the scope. Its size
// does not grow with the fleet.

// GRADES, worst first. A grade with attention is listed among the worst.
const GRADES = [
  { id: 'critical', label: 'Critical', attention: true },
  { id: 'warning', label: 'Warning', attention: true },
  { id: 'unreadable', label: 'Could not read', attention: true },
  { id: 'incomplete', label: 'Checks could not run', attention: true },
  { id: 'running', label: 'Running', attention: true },
  { id: 'info', label: 'Info only', attention: false },
  { id: 'clean', label: 'Clean', attention: false },
]
const RANK = Object.fromEntries(GRADES.map((g, i) => [g.id, i]))
const muted = { color: 'var(--text-secondary)' }

// gradeOf grades one first-look entry and counts its findings. A database
// whose checks could not run is never called clean.
function gradeOf(entry) {
  if (entry?.error) return { grade: 'unreadable', critical: 0, warning: 0 }
  const report = entry?.report
  if (!report) return { grade: 'running', critical: 0, warning: 0 }
  const items = report.items || []
  const count = sev => items.filter(i => i?.severity === sev).length
  const critical = count('critical')
  const warning = count('warning')
  let grade = 'clean'
  if (critical > 0) grade = 'critical'
  else if (warning > 0) grade = 'warning'
  else if ((report.checks || []).some(c => c?.status === 'degraded')) grade = 'incomplete'
  else if (items.length > 0) grade = 'info'
  return { grade, critical, warning }
}

// fleetGrades is the per-grade counts and the attention list, worst first:
// by grade, then critical and warning counts (most first), then name.
function fleetGrades(entries) {
  const graded = entries.map(entry => ({ database: entry.database, ...gradeOf(entry) }))
  const counts = GRADES.map(g => ({ ...g, n: graded.filter(e => e.grade === g.id).length }))
    .filter(g => g.n > 0)
  const attention = GRADES.filter(g => g.attention).map(g => g.id)
  const worst = graded.filter(e => attention.includes(e.grade)).sort((a, b) =>
    RANK[a.grade] - RANK[b.grade] || b.critical - a.critical || b.warning - a.warning
    || String(a.database).localeCompare(String(b.database)))
  return { counts, worst }
}

function findingsText(row) {
  const parts = []
  if (row.critical) parts.push(`${row.critical} critical`)
  if (row.warning) parts.push(`${row.warning} warning`)
  return parts.join(', ')
}

function WorstRow({ row, onSelectDatabase }) {
  const label = GRADES[RANK[row.grade]].label
  const findings = findingsText(row)
  return (
    <li data-testid="first-look-worst-row" data-db-row="" data-database={row.database}
      className="flex flex-wrap items-baseline gap-2 text-sm">
      <button type="button" className="font-mono underline"
        onClick={() => onSelectDatabase?.(row.database)}>{row.database}</button>
      <span className="text-xs" style={muted}>{label}{findings ? `: ${findings}` : ''}</span>
    </li>
  )
}

export function FirstLookSummary({ entries, onSelectDatabase }) {
  const { counts, worst } = fleetGrades(entries)
  const listed = worst.slice(0, SUMMARY_LIST)
  const more = worst.length - listed.length
  return (
    <section className="space-y-3" aria-labelledby="first-look-heading"
      data-testid="first-look-summary">
      <h2 id="first-look-heading" className="text-lg font-semibold">First look</h2>
      <p className="text-xs" style={muted}>
        {entries.length.toLocaleString('en-US')} databases, catalog only. Select one to see
        its report.
      </p>
      <ul aria-label="First look by grade" className="flex flex-wrap gap-3 text-sm">
        {counts.map(g => <li key={g.id} data-grade={g.id}>{g.label}: {g.n}</li>)}
      </ul>
      {listed.length === 0 ? (
        <p className="text-sm">No database needs attention.</p>
      ) : (
        <ol aria-label="Databases that need attention" className="space-y-1">
          {listed.map(row => <WorstRow key={row.database} row={row}
            onSelectDatabase={onSelectDatabase} />)}
        </ol>
      )}
      {more > 0 && <p className="text-xs" style={muted}>{more.toLocaleString('en-US')} more</p>}
    </section>
  )
}
