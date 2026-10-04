import { useAPI } from '../../hooks/useAPI'

// FirstLookPanel shows the catalog-only first look: what a DBA would flag
// on day one, each finding with the catalog evidence it rests on. Suggested
// SQL is for the operator to review; pg_sage never runs it.

const severityColor = {
  critical: 'var(--red)',
  warning: 'var(--yellow)',
  info: 'var(--text-secondary)',
}

function firstLookURL(database) {
  if (!database || database === 'all') return '/api/v1/first-look'
  return `/api/v1/first-look?database=${encodeURIComponent(database)}`
}

function Evidence({ evidence }) {
  if (!evidence?.length) return null
  return (
    <details className="mt-2 text-xs">
      <summary className="cursor-pointer" style={{ color: 'var(--text-secondary)' }}>
        Evidence
      </summary>
      <ul className="mt-1 space-y-1 font-mono">
        {evidence.map((e, i) => (
          <li key={i}>{e.ref}{e.detail ? ` — ${e.detail}` : ''}</li>
        ))}
      </ul>
    </details>
  )
}

function FindingCard({ item }) {
  return (
    <article className="rounded border p-3"
      style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
      <div className="flex items-start gap-2">
        <span className="text-xs font-semibold uppercase"
          style={{ color: severityColor[item.severity] || 'var(--text-secondary)' }}>
          {item.severity}
        </span>
        <h3 className="text-sm font-semibold">{item.title}</h3>
      </div>
      {item.detail && <p className="mt-1 text-sm">{item.detail}</p>}
      {item.recommendation && (
        <p className="mt-1 text-sm" style={{ color: 'var(--text-secondary)' }}>
          {item.recommendation}
        </p>
      )}
      {item.caveat && (
        <p className="mt-1 text-xs italic" style={{ color: 'var(--text-secondary)' }}>
          {item.caveat}
        </p>
      )}
      {item.suggested_sql && (
        <div className="mt-2">
          <pre className="overflow-x-auto rounded p-2 text-xs"
            style={{ background: 'var(--bg-primary)' }}>{item.suggested_sql}</pre>
          <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
            Review before running it yourself: pg_sage never runs it.
          </p>
        </div>
      )}
      <Evidence evidence={item.evidence} />
    </article>
  )
}

function Report({ entry }) {
  const report = entry.report
  if (!report) {
    return (
      <p role="status" className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        First look running for {entry.database}…
      </p>
    )
  }
  const items = report.items || []
  return (
    <div className="space-y-2">
      <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
        {entry.database}: {Number(report.relations || 0).toLocaleString('en-US')} relations
        read in {report.duration_ms} ms, catalog only.
      </p>
      {report.summary && <p className="text-sm font-medium">{report.summary}</p>}
      {items.length === 0 ? (
        <p className="text-sm">No problems found in the catalog.</p>
      ) : items.map((item, i) => (
        <FindingCard key={`${item.rule}-${item.object}-${i}`} item={item} />
      ))}
    </div>
  )
}

export function FirstLookPanel({ database }) {
  const { data } = useAPI(firstLookURL(database), 30000)
  const entries = data?.databases || []
  if (entries.length === 0) return null
  return (
    <section className="space-y-3" aria-labelledby="first-look-heading">
      <h2 id="first-look-heading" className="text-lg font-semibold">First look</h2>
      {entries.map(entry => <Report key={entry.database} entry={entry} />)}
    </section>
  )
}
