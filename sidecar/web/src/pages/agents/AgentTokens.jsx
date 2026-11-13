// An agent's MCP tokens (admin): the ones bound to this principal, each
// live one revocable in a ConfirmDialog (agentgov-07, dashboard-10b), and a
// new one (read, optionally propose; never approve), shown once.
import { useCallback, useEffect, useState } from 'react'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { useConfirmTarget } from '../../hooks/useConfirmTarget'
import { SecretNotice } from '../mcptokens/SecretNotice'
import { revocable, tokenStatus } from '../mcptokens/tokenApi'
import { listAgentTokens, mintAgentToken, revokeAgentToken, tokenBody } from './agentsApi'
import { Button, ErrorBox, ErrorText, Section, TextInput, muted, when } from './ui'

const emptyForm = { name: '', propose: false, databases: '', days: '30' }

function useTokens(id) {
  const [state, setState] = useState({ tokens: [], error: null, loaded: false })
  const load = useCallback(async () => {
    try {
      const tokens = await listAgentTokens(id)
      setState({ tokens, error: null, loaded: true })
    } catch (err) {
      setState(prev => ({ ...prev, tokens: [], error: err, loaded: true }))
    }
  }, [id])
  useEffect(() => { load() }, [load])
  return { ...state, reload: load }
}

function TokenList({ tokens, onRevoke, busy }) {
  if (tokens.length === 0) {
    return <p className="text-sm mb-3" style={muted}>No tokens for this agent.</p>
  }
  return (
    <ul data-testid="agent-tokens" className="text-sm mb-3">
      {tokens.map(t => (
        <li key={t.id} data-testid={`agent-token-${t.name}`} className="mb-1"
          style={{ color: 'var(--text-primary)' }}>
          {t.name} ({t.scopes.join(', ')}) on {t.databases.join(', ')}, expires{' '}
          {when(t.expires_at)}{t.revoked_at ? ', revoked' : ''}
          {revocable(tokenStatus(t)) && (
            <span className="ml-2">
              <Button testId={`agent-token-revoke-${t.name}`} onClick={() => onRevoke(t)}
                disabled={busy}>
                Revoke
              </Button>
            </span>
          )}
        </li>
      ))}
    </ul>
  )
}

// RevokeDialog asks before a token is revoked (dashboard-10b); a failure
// stays in the open dialog and the token stays listed.
function RevokeDialog({ rv }) {
  const t = rv.target
  return (
    <ConfirmDialog open={t !== null} title={t ? `Revoke token ${t.name}` : ''}
      action="Revoke" consequence="Its holder loses access at once."
      busy={rv.busy} onConfirm={rv.accept} onCancel={rv.cancel}
      error={rv.error && <ErrorText testId="agent-token-revoke-error" error={rv.error} />} />
  )
}

function MintForm({ agent, onMinted }) {
  const [form, setForm] = useState(emptyForm)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const set = key => value => setForm(prev => ({ ...prev, [key]: value }))
  const body = tokenBody(form)
  async function submit() {
    setBusy(true)
    setError(null)
    try {
      const tok = await mintAgentToken(agent.id, body)
      setForm(emptyForm)
      onMinted({ name: tok.name, token: tok.token })
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="flex flex-wrap gap-2 items-center">
      <TextInput testId="agent-token-name" value={form.name} onChange={set('name')}
        placeholder="token name" />
      <label className="text-sm" style={muted}>
        <input type="checkbox" data-testid="agent-token-scope-propose"
          checked={form.propose} onChange={e => set('propose')(e.target.checked)} />
        {' '}propose
      </label>
      <TextInput testId="agent-token-databases" value={form.databases}
        onChange={set('databases')} placeholder="databases (blank: all)" />
      <TextInput testId="agent-token-days" value={form.days} onChange={set('days')}
        placeholder="days (1-90)" />
      <Button testId="agent-token-submit" onClick={submit} disabled={busy || !body}>
        Issue token
      </Button>
      <ErrorBox testId="agent-token-error" error={error} />
    </div>
  )
}

export function AgentTokens({ agent, user }) {
  if (user?.role !== 'admin') {
    return (
      <Section title="Tokens" testId="agent-tokens-section">
        <p data-testid="agent-tokens-admin-only" className="text-sm" style={muted}>
          An admin issues and lists this agent&apos;s tokens.
        </p>
      </Section>
    )
  }
  return <AdminTokens agent={agent} />
}

function AdminTokens({ agent }) {
  const list = useTokens(agent.id)
  const rv = useConfirmTarget({ perform: t => revokeAgentToken(t.id), onDone: list.reload })
  const [created, setCreated] = useState(null)
  return (
    <Section title="Tokens" testId="agent-tokens-section">
      <SecretNotice created={created} onDismiss={() => setCreated(null)} />
      <ErrorBox testId="agent-tokens-error" error={list.error} />
      <RevokeDialog rv={rv} />
      {list.loaded && !list.error && <TokenList tokens={list.tokens} onRevoke={rv.open}
        busy={rv.busy} />}
      {agent.status !== 'retired' && (
        <MintForm agent={agent} onMinted={tok => { setCreated(tok); list.reload() }} />
      )}
    </Section>
  )
}
