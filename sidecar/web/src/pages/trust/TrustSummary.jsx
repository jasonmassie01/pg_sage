import { SUMMARY_LIST } from '../../lib/fleetScope'

// TrustSummary is the trust ledger of a whole fleet (ux-10b): for each
// family and class, how many databases hold each level, and the databases
// off the usual level (the exceptions). Approving a promotion and
// re-approving a capped level stay on one database's Trust page.

const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }
const muted = { color: 'var(--text-secondary)' }

// fleetClasses folds every database's rows into one entry per family and
// class: level counts, the usual (most common) level and the exceptions.
function fleetClasses(views) {
  const byClass = new Map()
  let pending = 0
  for (const view of views) {
    for (const row of view.rows || []) {
      if (row.pending) pending += 1
      const key = `${row.family}/${row.class}`
      if (!byClass.has(key)) {
        byClass.set(key, { family: row.family, cls: row.class, levels: {}, holders: [] })
      }
      const entry = byClass.get(key)
      const level = row.level || 'unknown'
      entry.levels[level] = (entry.levels[level] || 0) + 1
      entry.holders.push({ database: view.database, level })
    }
  }
  const classes = [...byClass.values()].sort((a, b) =>
    `${a.family}/${a.cls}`.localeCompare(`${b.family}/${b.cls}`))
  for (const entry of classes) {
    const levels = Object.entries(entry.levels).sort(([a], [b]) => a.localeCompare(b))
    entry.levelList = levels
    entry.usual = levels.reduce((best, cur) => (cur[1] > best[1] ? cur : best))[0]
    entry.exceptions = entry.holders.filter(h => h.level !== entry.usual)
      .sort((a, b) => String(a.database).localeCompare(String(b.database)))
  }
  return { classes, pending }
}

function ClassRow({ entry, onSelectDatabase }) {
  const listed = entry.exceptions.slice(0, SUMMARY_LIST)
  const more = entry.exceptions.length - listed.length
  return (
    <li data-testid={`trust-class-${entry.family}-${entry.cls}`} className="text-sm space-y-1">
      <div className="flex flex-wrap gap-3">
        <span className="font-medium">{entry.family} / {entry.cls}</span>
        <span style={muted}>{entry.levelList.map(([l, n]) => `${l} ${n}`).join(' · ')}</span>
      </div>
      {listed.length > 0 && (
        <ul aria-label={`Exceptions for ${entry.family} / ${entry.cls}`}
          className="flex flex-wrap gap-x-3 text-xs">
          {listed.map(h => (
            <li key={h.database} data-testid="trust-exception" data-db-row="">
              <button type="button" className="font-mono underline"
                onClick={() => onSelectDatabase?.(h.database)}>{h.database}</button>
              {' '}{h.level}
            </li>
          ))}
          {more > 0 && <li style={muted}>{more} more</li>}
        </ul>
      )}
    </li>
  )
}

export function TrustSummary({ views, onSelectDatabase }) {
  const { classes, pending } = fleetClasses(views)
  return (
    <div className="rounded border p-3 space-y-3" style={card} data-testid="trust-summary">
      <h3 className="text-sm font-semibold">Levels across {views.length} databases</h3>
      <p className="text-xs" style={muted}>
        Each class lists the databases off its usual level. Select a database to see its
        evidence, approve a promotion or re-approve a level.
      </p>
      {pending > 0 && (
        <p className="text-sm" data-testid="trust-summary-pending">
          {pending} promotion{pending === 1 ? ' is' : 's are'} waiting for an admin.
        </p>
      )}
      <ul className="space-y-2">
        {classes.map(entry => <ClassRow key={`${entry.family}/${entry.cls}`} entry={entry}
          onSelectDatabase={onSelectDatabase} />)}
      </ul>
    </div>
  )
}
