// rules-12b: the Cases page can ask the server for one subsystem's
// findings (GET /api/v1/cases?finding_source=<subsystem>). The server
// filters before its row cap and lists the accepted values in
// finding_sources, so the page holds no copy of the list.

// FindingSourceSelect picks one finding source, or All ('').
export function FindingSourceSelect({ value, sources, onChange }) {
  const listed = Array.isArray(sources) ? sources : []
  const options = value && !listed.includes(value) ? [...listed, value] : listed
  return (
    <span className="inline-flex flex-wrap items-center gap-2 text-xs"
      style={{ color: 'var(--text-secondary)' }}>
      <label htmlFor="cases-finding-source">Issue source</label>
      <select id="cases-finding-source" data-testid="cases-finding-source"
        value={value || ''} onChange={e => onChange(e.target.value)}
        className="rounded px-2 py-1 text-xs"
        style={{ color: 'var(--text-primary)', border: '1px solid var(--border)',
          background: 'var(--bg-card)' }}>
        <option value="">All</option>
        {options.map(source => <option key={source} value={source}>{source}</option>)}
      </select>
      {value && <span data-testid="finding-source-note">
        Showing {value} issues only; incidents and query hints are hidden.
      </span>}
    </span>
  )
}
