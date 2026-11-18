// CasesHealthNotes says what the case list is missing: one banner per
// database whose cases could not be read, and a note per database whose
// list was cut at its cap (cases-05).
const ERROR_CLASS_TEXT = {
  timeout: 'did not answer in time',
  connection: 'could not be reached',
  query: 'returned an error while reading its cases',
}

const SOURCE_TEXT = {
  findings: 'findings',
  incidents: 'incidents',
  query_hints: 'query hints',
}

export function CasesHealthNotes({ unavailable, truncated }) {
  const down = Array.isArray(unavailable) ? unavailable : []
  const cut = Object.entries(truncated || {})
    .map(([database, sources]) => [database, cutSources(sources)])
    .filter(([, sources]) => sources.length > 0)
  if (down.length === 0 && cut.length === 0) return null
  return (
    <div className="space-y-2">
      {down.map(item => (
        <div key={item.database} role="alert" data-testid="cases-unavailable"
          className="rounded border p-3 text-sm"
          style={{ borderColor: 'var(--danger, var(--border))',
            color: 'var(--text-primary)' }}>
          Issues for database <strong>{item.database}</strong>{' '}
          {ERROR_CLASS_TEXT[item.error_class] || 'could not be read'}. Its issues are
          missing from this list; the other databases are shown.
        </div>
      ))}
      {cut.map(([database, sources]) => (
        <p key={database} data-testid="cases-truncated" className="text-xs"
          style={{ color: 'var(--text-secondary)' }}>
          Database {database} has more open {sources.join(', ')} than this list
          shows. The most severe are listed; open an issue link to load one that
          is not shown.
        </p>
      ))}
    </div>
  )
}

function cutSources(sources) {
  return Object.keys(SOURCE_TEXT)
    .filter(key => sources?.[key])
    .map(key => SOURCE_TEXT[key])
}
