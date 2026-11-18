import { useEffect, useState } from 'react'

// FleetFindings lists the problems open on several fleet databases at once
// (fleet learning), each once with a drill-down to the databases it affects;
// each occurrence links to that database's finding case (fleet-06).
export function FleetFindings({ onSelectDatabase }) {
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  const [open, setOpen] = useState({})

  useEffect(() => {
    let cancelled = false
    async function load() {
      try {
        const res = await fetch('/api/v1/fleet/findings', {
          credentials: 'include',
        })
        if (!res.ok) throw new Error(`fleet findings: HTTP ${res.status}`)
        const body = await res.json()
        if (!cancelled) { setData(body); setError(null) }
      } catch (err) {
        if (!cancelled) setError(err.message)
      }
    }
    load()
    return () => { cancelled = true }
  }, [])

  const findings = Array.isArray(data?.findings) ? data.findings : []
  const errors = Array.isArray(data?.errors) ? data.errors : []
  const truncated = Array.isArray(data?.truncated) ? data.truncated : []
  const min = data?.min_databases || 3

  return (
    <section className="mt-6" data-testid="fleet-findings">
      <h2 className="text-sm font-semibold mb-2"
        style={{ color: 'var(--text-primary)' }}>
        Recurring across the fleet
      </h2>
      {error && (
        <p className="text-sm" data-testid="fleet-findings-error"
          style={{ color: 'var(--red)' }}>
          Could not load fleet issues: {error}
        </p>
      )}
      {!error && data && findings.length === 0 && (
        <p className="text-sm" data-testid="fleet-findings-empty"
          style={{ color: 'var(--text-secondary)' }}>
          No open problem recurs on {min} or more databases.
        </p>
      )}
      {errors.length > 0 && (
        <p className="text-xs mb-2" data-testid="fleet-findings-partial"
          style={{ color: 'var(--text-secondary)' }}>
          Not read: {errors.map(e => e.database).join(', ')}
        </p>
      )}
      {truncated.length > 0 && (
        <p className="text-xs mb-2" data-testid="fleet-findings-truncated"
          style={{ color: 'var(--text-secondary)' }}>
          More open issues than read; counts may be low for:{' '}
          {truncated.join(', ')}
        </p>
      )}
      {findings.length > 0 && (
        <div className="rounded-lg divide-y" style={{
          background: 'var(--bg-card)', border: '1px solid var(--border)',
        }}>
          {findings.map((f, i) => (
            <FleetFindingRow key={f.key || i} finding={f} index={i}
              open={!!open[i]} onSelectDatabase={onSelectDatabase}
              onToggle={() => setOpen(o => ({ ...o, [i]: !o[i] }))} />
          ))}
        </div>
      )}
    </section>
  )
}

// findingLink opens one database's finding on the Cases page.
function findingLink(occurrence) {
  const query = new URLSearchParams({
    database: occurrence.database, finding: String(occurrence.id),
  })
  return `#/cases?${query}`
}

function FleetFindingRow({ finding, index, open, onToggle, onSelectDatabase }) {
  const occurrences = Array.isArray(finding.occurrences)
    ? finding.occurrences : []
  return (
    <div className="px-4 py-3 text-sm">
      <button type="button" className="flex w-full items-center
        justify-between text-left" onClick={onToggle}
        data-testid={`fleet-finding-toggle-${index}`}
        aria-expanded={open}>
        <span style={{ color: 'var(--text-primary)' }}>
          {finding.title || finding.object_identifier}
          <span className="ml-2 text-xs"
            style={{ color: 'var(--text-secondary)' }}>
            {finding.category} · {finding.severity}
          </span>
        </span>
        <span data-testid={`fleet-finding-count-${index}`}
          style={{ color: 'var(--text-secondary)' }}>
          {finding.databases} databases
        </span>
      </button>
      {open && (
        <ul className="mt-2 ml-4 list-disc">
          {occurrences.map(o => (
            <li key={`${o.database}-${o.id}`}
              style={{ color: 'var(--text-secondary)' }}>
              {Number.isInteger(o.id) && o.id > 0 ? (
                <a href={findingLink(o)} className="underline"
                  style={{ color: 'var(--accent)' }}
                  onClick={() => onSelectDatabase?.(o.database)}>
                  <span>{o.database}</span>
                  {o.severity ? ` · ${o.severity}` : ''}
                  {` · issue ${o.id}`}
                </a>
              ) : (
                <>
                  <span>{o.database}</span>
                  {o.severity ? ` · ${o.severity}` : ''}
                </>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
