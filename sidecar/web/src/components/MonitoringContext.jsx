const muted = { color: 'var(--text-secondary)' }

function collectionTime(value) {
  const time = new Date(value || '')
  return Number.isNaN(time.getTime()) || time.getUTCFullYear() < 1971 ? null : time
}

// This describes reported collection and policy state, not database health.
// A connection alone is not evidence that every collector has succeeded.
export function MonitoringContext({ database, fleetData, error, user }) {
  const rows = fleetData?.databases
  const selected = database && database !== 'all'
    ? rows?.find(row => row.name === database) : null
  const status = selected?.status
  const mode = status?.trust_level
  const collected = collectionTime(status?.collector_last_run)
  const observationCount = rows?.filter(row => row.status?.trust_level === 'observation').length
  return (
    <aside data-testid="monitoring-context" aria-label="Monitoring scope and execution mode"
      className="rounded border p-3 mb-4 text-sm space-y-1"
      style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
      <p><strong>{database && database !== 'all' ? database : 'All databases'}</strong>
        {' · '}{selected?.emergency_stopped ? 'Execution stopped' : mode ? `${mode[0].toUpperCase()}${mode.slice(1)} mode` : selected ? 'Execution policy unknown' : 'Fleet scope'}</p>
      <p style={muted}>
        {selected?.emergency_stopped
          ? 'The emergency stop blocks database changes. Review evidence and execution settings in Trust.'
          : mode === 'observation'
            ? 'Database changes are disabled in Observation mode. Review Cases and proposed changes; no database changes will run in this mode.'
            : mode
              ? 'Execution depends on the action’s policy, permissions and safety checks. Review its availability before approving a change.'
              : selected
                ? 'This database has not reported its execution policy. Review its status in Fleet before deciding on a change.'
                : rows?.length
                ? `${observationCount} of ${rows.length} databases use Observation mode. Select a database to see its execution policy and collection time.`
                : 'Waiting for database status. Execution policy and evidence freshness are not established.'}
      </p>
      {selected && <p className="text-xs" style={muted}>
        {status?.connected === true ? 'Connected' : 'Connection not confirmed'}
        {' · '}{collected
          ? <>Last collected <time dateTime={collected.toISOString()}>{collected.toLocaleString()}</time>. Collection timestamps do not verify every signal.</>
          : 'No collection timestamp reported. Evidence freshness is unknown.'}
      </p>}
      {(error || status?.error || (selected && status?.connected !== true)) && (
        <p role="status" className="text-xs" style={{ color: 'var(--amber, #f0a54a)' }}>
          Monitoring status is incomplete or could not be refreshed. Existing cases do not establish current health.
          {' '}{user?.role === 'admin'
            ? <a href="#/manage-databases" className="underline">Review Fleet</a>
            : 'Ask an admin to review the database’s collection status in Fleet.'}
        </p>
      )}
    </aside>
  )
}
