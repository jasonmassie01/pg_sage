// An agent's grants and grant requests on one database (spec §6.6; G1:
// every grant is L2, operator-approved). Operators approve or deny a
// request and revoke a grant; a refusal shows its reason and the exact
// statement to run.
import { useCallback, useEffect, useState } from 'react'
import { agentGrants, decideRequest, grantRequests, revokeGrant } from './agentsApi'
import { Button, ErrorBox, Section, inputStyle, muted, when } from './ui'

function useGrants(agentID, database) {
  const [state, setState] = useState({ grants: [], requests: [], error: null,
    loaded: false })
  const load = useCallback(async () => {
    try {
      const [g, r] = await Promise.all([agentGrants(agentID, database),
        grantRequests(agentID, database)])
      setState({ grants: g?.items || [], requests: r?.items || [], error: null,
        loaded: true })
    } catch (err) {
      setState(prev => ({ ...prev, grants: [], requests: [], error: err, loaded: true }))
    }
  }, [agentID, database])
  useEffect(() => { load() }, [load])
  return { ...state, reload: load }
}

function objectsText(objects) {
  return (objects || []).map(o => (o.columns?.length
    ? `${o.object} (${o.columns.join(', ')})` : o.object)).join('; ')
}

function GrantRow({ g, onRevoke, busy }) {
  return (
    <li data-testid={`agent-grant-${g.id}`} className="text-sm mb-1"
      style={{ color: 'var(--text-primary)' }}>
      {g.capability}: {g.privileges.join(', ')} on {g.object}
      {g.columns?.length ? ` (${g.columns.join(', ')})` : ''}, {g.state}, expires{' '}
      {when(g.expires_at)}
      {g.revoke_detail && <span style={muted}> — {g.revoke_detail}</span>}
      {g.state === 'active' && (
        <span className="ml-2">
          <Button testId={`agent-grant-revoke-${g.id}`} onClick={() => onRevoke(g)}
            disabled={busy}>
            Revoke
          </Button>
        </span>
      )}
    </li>
  )
}

function RequestRow({ r, onDecide, busy }) {
  return (
    <li data-testid={`agent-request-${r.id}`} className="text-sm mb-1"
      style={{ color: 'var(--text-primary)' }}>
      #{r.id} {r.capability} on {objectsText(r.objects)} for {r.duration_minutes} min:{' '}
      &ldquo;{r.reason}&rdquo; ({r.status})
      {r.status === 'pending' && (
        <span className="ml-2 inline-flex gap-2">
          <Button testId={`agent-request-approve-${r.id}`} disabled={busy}
            onClick={() => onDecide(r, 'approve')}>Approve</Button>
          <Button testId={`agent-request-deny-${r.id}`} disabled={busy}
            onClick={() => onDecide(r, 'deny')}>Deny</Button>
        </span>
      )}
    </li>
  )
}

function useActions(agentID, database, reload) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  async function run(fn) {
    setBusy(true)
    setError(null)
    try {
      await fn()
      await reload()
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }
  const revoke = g => {
    if (!confirm(`Revoke grant #${g.id} on ${g.object}? The agent loses it now.`)) return
    run(() => revokeGrant(agentID, database, g.id))
  }
  const decide = (r, verb) => run(() => decideRequest(agentID, database, r.id, verb))
  return { busy, error, revoke, decide }
}

function GrantLists({ list, actions }) {
  if (list.error) {
    return (
      <p data-testid="agent-grants-unavailable" className="text-sm" style={muted}>
        Grants are unavailable here: {list.error.message}
      </p>
    )
  }
  if (!list.loaded) return null
  return (
    <>
      <ErrorBox testId="agent-grants-error" error={actions.error} />
      <h4 className="text-xs mb-1" style={muted}>Grants</h4>
      {list.grants.length === 0
        ? <p className="text-sm mb-2" style={muted}>No grants on this database.</p>
        : (
          <ul data-testid="agent-grants" className="mb-3">
            {list.grants.map(g => <GrantRow key={g.id} g={g} busy={actions.busy}
              onRevoke={actions.revoke} />)}
          </ul>
        )}
      <h4 className="text-xs mb-1" style={muted}>Requests</h4>
      {list.requests.length === 0
        ? <p className="text-sm" style={muted}>No requests on this database.</p>
        : (
          <ul data-testid="agent-requests">
            {list.requests.map(r => <RequestRow key={r.id} r={r} busy={actions.busy}
              onDecide={actions.decide} />)}
          </ul>
        )}
    </>
  )
}

function GrantsOn({ agent, databases }) {
  const [database, setDatabase] = useState(databases[0])
  const list = useGrants(agent.id, database)
  const actions = useActions(agent.id, database, list.reload)
  return (
    <>
      <select data-testid="agent-grants-database" value={database} aria-label="Database"
        onChange={e => setDatabase(e.target.value)}
        className="px-2 py-1 rounded text-sm mb-3" style={inputStyle}>
        {databases.map(d => <option key={d} value={d}>{d}</option>)}
      </select>
      <GrantLists list={list} actions={actions} />
    </>
  )
}

export function AgentGrants({ agent, databases }) {
  return (
    <Section title="Grants" testId="agent-grants-section">
      {databases.length === 0 ? (
        <p data-testid="agent-grants-unavailable" className="text-sm" style={muted}>
          No monitored database to show grants for.
        </p>
      ) : <GrantsOn agent={agent} databases={databases} />}
    </Section>
  )
}
