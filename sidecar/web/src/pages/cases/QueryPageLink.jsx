// QueryPageLink opens the workload explorer's page of the query a case's
// evidence names (market-01): its history, plan and related cases.
export function QueryPageLink({ queryid, database }) {
  if (!queryid || !/^-?\d+$/.test(String(queryid))) return null
  const params = new URLSearchParams({ queryid: String(queryid) })
  if (database) params.set('database', database)
  return (
    <a href={`#/query?${params}`} data-testid="case-query-link"
      className="inline-block mt-2 text-xs underline" style={{ color: 'var(--accent)' }}>
      Open the query page (history and plan)
    </a>
  )
}
