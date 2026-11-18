import {
  fleetAutonomy, normalizeTrustLevel, rowAutoFamilies, unreportedNote,
} from '../lib/autonomy'
import { FLEET_SUMMARY_FROM } from '../lib/fleetScope'
import { formatTime } from '../lib/time'

const muted = { color: 'var(--text-secondary)' }

function collectionTime(value) {
  const time = new Date(value || '')
  return Number.isNaN(time.getTime()) || time.getUTCFullYear() < 1971 ? null : time
}

// selectedPolicy states what one non-observation database may run without
// approval now, from its reported action families (dashboard-02).
function selectedPolicy(row) {
  const families = rowAutoFamilies(row)
  if (families === undefined) {
    return 'Execution depends on the action’s policy, permissions and safety checks.'
      + ' Review its availability before approving a change.'
  }
  if (families.length === 0) {
    return 'Nothing runs without approval right now.'
      + ' Changes queue for approval or are blocked by policy.'
  }
  return `Can run without approval now: ${families.join(', ')}.`
    + ' Everything else queues for approval or is blocked by policy.'
}

// onWhich names the databases that run a family, or from FLEET_SUMMARY_FROM
// databases up counts them, so the strip does not grow with the fleet
// (ux-10b).
function onWhich(names, fleetSize) {
  if (fleetSize < FLEET_SUMMARY_FROM) return names.join(', ')
  return `${names.length} database${names.length === 1 ? '' : 's'}`
}

// fleetAutoClaim states what the fleet may run without approval now. While
// some databases have not reported, the claim covers only those that did
// and says how many are missing, since an unreported one may still execute.
function fleetAutoClaim(autonomy) {
  const { autoByFamily, reported, total } = autonomy
  const missing = unreportedNote(autonomy)
  const families = Object.keys(autoByFamily).sort()
  const parts = []
  if (families.length > 0) {
    parts.push('Can run without approval now: ' + families
      .map(f => `${f} on ${onWhich(autoByFamily[f], total)}`).join('; ') + '.')
  } else if (missing) {
    parts.push('Of the databases that reported their action policy,'
      + ' none runs anything without approval right now.')
  } else if (reported > 0) {
    parts.push('Nothing runs without approval right now.')
  }
  if (missing) parts.push(`${missing}.`)
  return parts
}

// fleetPolicy summarises the fleet: observation count, stopped count and,
// per family, the databases that run it unattended.
function fleetPolicy(rows, summary) {
  const autonomy = fleetAutonomy(rows)
  const parts = [`${autonomy.levels.observation} of ${rows.length} databases use Observation mode.`]
  const stopped = summary?.emergency_stopped_count
  if (Number.isInteger(stopped) && stopped > 0) {
    parts.push(`${stopped} of ${rows.length} databases emergency-stopped.`)
  }
  parts.push(...fleetAutoClaim(autonomy))
  parts.push('Select a database to see its execution policy and collection time.')
  return parts.join(' ')
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
  return (
    <aside data-testid="monitoring-context" aria-label="Monitoring scope and autonomy"
      className="rounded border p-3 mb-4 text-sm space-y-1"
      style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
      <p><strong>{database && database !== 'all' ? database : 'All databases'}</strong>
        {' · '}{selected?.emergency_stopped ? 'Execution stopped' : mode ? `${mode[0].toUpperCase()}${mode.slice(1)} mode` : selected ? 'Execution policy unknown' : 'Fleet scope'}</p>
      <p style={muted}>
        {selected?.emergency_stopped
          ? 'The emergency stop blocks database changes. Review evidence and execution settings in Trust.'
          : normalizeTrustLevel(mode) === 'observation'
            ? 'Database changes are disabled in Observation mode. Review issues and proposed changes; no database changes will run in this mode.'
            : mode
              ? selectedPolicy(selected)
              : selected
                ? 'This database has not reported its execution policy. Review its status in Fleet before deciding on a change.'
                : rows?.length
                ? fleetPolicy(rows, fleetData?.summary)
                : 'Waiting for database status. Execution policy and evidence freshness are not established.'}
      </p>
      {selected && <p className="text-xs" style={muted}>
        {status?.connected === true ? 'Connected' : 'Connection not confirmed'}
        {' · '}{collected
          ? <>Last collected <time dateTime={collected.toISOString()}>{formatTime(collected)}</time>. Collection timestamps do not verify every signal.</>
          : 'No collection timestamp reported. Evidence freshness is unknown.'}
      </p>}
      {(error || status?.error || (selected && status?.connected !== true)) && (
        <p role="status" className="text-xs" style={{ color: 'var(--amber, #f0a54a)' }}>
          Monitoring status is incomplete or could not be refreshed. Existing issues do not establish current health.
          {' '}{user?.role === 'admin'
            ? <a href="#/manage-databases" className="underline">Review Fleet</a>
            : 'Ask an admin to review the database’s collection status in Fleet.'}
        </p>
      )}
    </aside>
  )
}
