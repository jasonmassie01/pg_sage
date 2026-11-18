import { useAPI } from '../../hooks/useAPI'
import { QueryState } from '../../components/QueryState'
import { formatTime } from '../../lib/time'

// The standing policy the gate decides under (policy-07), read-only: the
// current document and its ratified versions. Changes go through a
// proposal and an admin ratification (POST /api/v1/policy/proposals).

export const POLICY_GATE_DOC =
  'https://github.com/jasonmassie01/pg_sage/blob/master/docs/policy-gate.md'

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

function scoped(path, databaseId, extra) {
  const params = new URLSearchParams()
  if (databaseId) params.set('database_id', String(databaseId))
  if (extra) Object.entries(extra).forEach(([k, v]) => params.set(k, v))
  const query = params.toString()
  return `${path}${query ? `?${query}` : ''}`
}

function when(value) {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? 'unknown' : formatTime(date)
}

export function StandingPolicy({ databaseId }) {
  const current = useAPI(scoped('/api/v1/policy', databaseId))
  const history = useAPI(scoped('/api/v1/policy/history', databaseId, { limit: '20' }))
  return (
    <section className="space-y-3 text-sm" data-testid="standing-policy">
      <h3 className="font-medium" style={strong}>Standing policy</h3>
      <p style={muted}>
        What pg_sage may do unattended. Every decision and reason the gate records is
        explained in the <a href={POLICY_GATE_DOC} className="underline"
          style={{ color: 'var(--accent)' }}>policy check reference</a>.
      </p>
      <div data-testid="standing-policy-current">
        <QueryState query={current}>
          {data => data?.version
            ? <CurrentPolicy policy={data} />
            : <p style={muted}>No standing policy has been ratified yet.</p>}
        </QueryState>
      </div>
      <QueryState query={history}>
        {data => <ul className="space-y-1">
          {(data?.policies || []).map(p => <li key={p.id} data-testid="standing-policy-version"
            className="text-xs" style={muted}>
            Version {p.version} ({p.profile}) by {p.updated_by}, {when(p.updated_at)}
          </li>)}
        </ul>}
      </QueryState>
    </section>
  )
}

function CurrentPolicy({ policy }) {
  return <>
    <p style={strong}>Version {policy.version} · profile {policy.profile} · ratified by
      {' '}{policy.updated_by}, {when(policy.updated_at)}</p>
    <pre data-testid="standing-policy-document" className="mt-2 overflow-x-auto rounded p-2 text-xs"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)', ...muted }}>
      {JSON.stringify(policy.document, null, 2)}
    </pre>
  </>
}
