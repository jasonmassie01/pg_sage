// An agent's OAuth identities (admin, agentgov-03): the (issuer, subject)
// pairs bound to this principal, a bind form whose issuer is one of
// mcp.oauth.issuers, and unbind with confirmation (it takes effect at once).
import { useCallback, useEffect, useState } from 'react'
import {
  bindAgentIdentity, identityBody, listAgentIdentities, oauthIssuers, unbindAgentIdentity,
} from './agentsApi'
import { Button, ErrorBox, Section, TextInput, inputStyle, muted, when } from './ui'

const noIdentities = { items: [], truncated: false }
const noIssuers = { enabled: false, issuers: [] }

function useIdentities(id) {
  const [state, setState] = useState({ data: noIdentities, error: null, loaded: false })
  const load = useCallback(async () => {
    try {
      const data = await listAgentIdentities(id)
      setState({ data, error: null, loaded: true })
    } catch (err) {
      setState(prev => ({ ...prev, data: noIdentities, error: err, loaded: true }))
    }
  }, [id])
  useEffect(() => { load() }, [load])
  return { ...state, reload: load }
}

function useIssuers() {
  const [state, setState] = useState({ data: noIssuers, error: null, loaded: false })
  useEffect(() => {
    let live = true
    oauthIssuers().then(
      data => { if (live) setState({ data, error: null, loaded: true }) },
      err => { if (live) setState({ data: noIssuers, error: err, loaded: true }) })
    return () => { live = false }
  }, [])
  return state
}

function IdentityList({ list, onUnbind }) {
  const items = Array.isArray(list.items) ? list.items : []
  if (items.length === 0) {
    return (
      <p data-testid="agent-identities-empty" className="text-sm mb-3" style={muted}>
        No OAuth identities are bound to this agent.
      </p>
    )
  }
  return (
    <ul data-testid="agent-identities" className="text-sm mb-3">
      {items.map((b, i) => (
        <li key={`${b.issuer} ${b.subject}`} data-testid={`agent-identity-${i}`}
          className="flex flex-wrap gap-2 items-center mb-1"
          style={{ color: 'var(--text-primary)' }}>
          <span>{b.subject} at {b.issuer}, bound by {b.created_by}, {when(b.created_at)}</span>
          <Button testId={`agent-identity-unbind-${i}`} onClick={() => onUnbind(b)}>
            Unbind
          </Button>
        </li>
      ))}
      {list.truncated && <li style={muted}>Only the first 500 are shown.</li>}
    </ul>
  )
}

function BindForm({ agent, issuers, onBound }) {
  const [form, setForm] = useState({ issuer: issuers[0] || '', subject: '' })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const body = identityBody(form, issuers)
  async function submit() {
    setBusy(true)
    setError(null)
    try {
      await bindAgentIdentity(agent.id, body)
      setForm(prev => ({ ...prev, subject: '' }))
      onBound()
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="flex flex-wrap gap-2 items-center">
      <select data-testid="agent-identity-issuer" aria-label="issuer" value={form.issuer}
        onChange={e => setForm(prev => ({ ...prev, issuer: e.target.value }))}
        className="px-2 py-1 rounded text-sm" style={inputStyle}>
        {issuers.map(iss => <option key={iss} value={iss}>{iss}</option>)}
      </select>
      <TextInput testId="agent-identity-subject" value={form.subject}
        onChange={v => setForm(prev => ({ ...prev, subject: v }))}
        placeholder="subject (the agent's client id)" />
      <Button testId="agent-identity-submit" onClick={submit} disabled={busy || !body}>
        Bind identity
      </Button>
      <ErrorBox testId="agent-identity-error" error={error} />
    </div>
  )
}

function BindArea({ agent, issuers, onBound }) {
  if (!issuers.loaded || issuers.error) return null
  if (!issuers.data.enabled) {
    return (
      <p data-testid="agent-identities-oauth-off" className="text-sm" style={muted}>
        OAuth is off: set mcp.oauth.enabled and list the issuer in mcp.oauth.issuers.
      </p>
    )
  }
  if (agent.status !== 'active') {
    return (
      <p data-testid="agent-identities-inactive" className="text-sm" style={muted}>
        A {agent.status} agent gets no new identity.
      </p>
    )
  }
  return <BindForm agent={agent} issuers={issuers.data.issuers || []} onBound={onBound} />
}

function AdminIdentities({ agent }) {
  const list = useIdentities(agent.id)
  const issuers = useIssuers()
  const [error, setError] = useState(null)
  async function unbind(b) {
    if (!confirm(`Unbind ${b.subject} at ${b.issuer}? Its OAuth tokens are refused at once.`)) {
      return
    }
    setError(null)
    try {
      await unbindAgentIdentity(agent.id, b.issuer, b.subject)
      list.reload()
    } catch (err) {
      setError(err)
    }
  }
  return (
    <Section title="OAuth identities" testId="agent-identities-section">
      <ErrorBox testId="agent-identities-error" error={list.error || error || issuers.error} />
      {list.loaded && !list.error && <IdentityList list={list.data} onUnbind={unbind} />}
      <BindArea agent={agent} issuers={issuers} onBound={list.reload} />
    </Section>
  )
}

export function AgentIdentities({ agent, user }) {
  if (user?.role !== 'admin') {
    return (
      <Section title="OAuth identities" testId="agent-identities-section">
        <p data-testid="agent-identities-admin-only" className="text-sm" style={muted}>
          An admin binds and lists this agent&apos;s OAuth identities.
        </p>
      </Section>
    )
  }
  return <AdminIdentities agent={agent} />
}
