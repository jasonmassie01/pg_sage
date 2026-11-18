// MCP v2: admins list and revoke every MCP API token, and create operator
// tokens: a person's own token for Claude Code or Cursor, acting for its
// owner. A coding agent's token is minted per agent on the Agents page
// (agentgov-01). Admin only: it is the MCP tokens tab of Agents (ux-11),
// which other roles never get. Agent requests moved to the Inbox and ledger.
import { useCallback, useEffect, useState } from 'react'
import { useConfirm } from '../hooks/useConfirm'
import { SecretNotice } from './mcptokens/SecretNotice'
import { TokenForm } from './mcptokens/TokenForm'
import { TokenTable } from './mcptokens/TokenTable'
import { createToken, listTokens, revokeToken } from './mcptokens/tokenApi'
import { useOwners } from './mcptokens/useOwners'

const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
const errorStyle = {
  background: 'rgba(239,68,68,0.1)',
  color: 'var(--red)',
  border: '1px solid rgba(239,68,68,0.3)',
}

function Explainer() {
  return (
    <div data-testid="mcp-tokens-explainer" className="rounded-lg p-4 mb-4 text-sm"
      style={{ ...card, color: 'var(--text-secondary)' }}>
      <h2 className="text-sm font-semibold mb-1" style={{ color: 'var(--text-primary)' }}>
        MCP for coding agents
      </h2>
      <p>
        A token lets an MCP client such as Claude Code or Cursor read pg_sage&apos;s
        issues and propose fixes. The tokens created here are operator tokens: each
        acts for its owner, an operator or admin, and may approve. Agent tokens can
        read and propose but never approve. Every proposed change still goes through
        pg_sage&apos;s trust gates. See docs/mcp.md for setup and the tools each scope
        unlocks.
      </p>
    </div>
  )
}

function AgentsBanner() {
  return (
    <div data-testid="mcp-tokens-agents-banner" className="rounded-lg p-3 mb-4 text-sm"
      style={{ ...card, color: 'var(--text-primary)' }}>
      Tokens for coding agents are issued per agent on the Agents page.{' '}
      <a data-testid="mcp-tokens-agents-link" href="#/agents" className="underline"
        style={{ color: 'var(--accent)' }}>
        Open Agents
      </a>
    </div>
  )
}

function useTokenList() {
  const [state, setState] = useState({ tokens: [], loaded: false, error: null, at: 0 })
  const load = useCallback(async () => {
    try {
      const tokens = await listTokens()
      setState({ tokens, loaded: true, error: null, at: Date.now() })
    } catch (err) {
      setState(prev => ({ ...prev, loaded: true, error: err.message }))
    }
  }, [])
  useEffect(() => { load() }, [load])
  const replace = token => setState(prev => ({
    ...prev, tokens: prev.tokens.map(t => (t.id === token.id ? token : t)),
  }))
  return { ...state, load, replace }
}

function useRevoke(replace, setError) {
  const [revokingIds, setRevokingIds] = useState(() => new Set())
  const { askConfirm, dialog } = useConfirm()
  const mark = (id, on) => setRevokingIds(prev => {
    const next = new Set(prev)
    if (on) next.add(id)
    else next.delete(id)
    return next
  })
  async function revoke(token) {
    if (revokingIds.has(token.id)) return
    const ok = await askConfirm({
      title: `Revoke MCP token "${token.name}"`, action: 'Revoke',
      consequence: 'Agents using this token lose access now.',
    })
    if (!ok) return
    setError(null)
    mark(token.id, true)
    try {
      replace(await revokeToken(token.id))
    } catch (err) {
      setError(err.message)
    } finally {
      mark(token.id, false)
    }
  }
  return { revokingIds, revoke, dialog }
}

function useCreate(load) {
  const [created, setCreated] = useState(null)
  const [creating, setCreating] = useState(false)
  const [formError, setFormError] = useState(null)
  async function create(body) {
    setCreating(true)
    setFormError(null)
    try {
      const token = await createToken(body)
      setCreated({ name: token.name, token: token.token })
      load()
      return true
    } catch (err) {
      setFormError(err.message)
      return false
    } finally {
      setCreating(false)
    }
  }
  return { created, creating, formError, create, dismiss: () => setCreated(null) }
}

function TokenList({ list, owners, revokingIds, onRevoke }) {
  if (list.error) {
    return (
      <div data-testid="mcp-tokens-error" className="text-sm p-3 rounded" style={errorStyle}>
        Could not load MCP tokens: {list.error}
      </div>
    )
  }
  if (!list.loaded) return null
  if (list.tokens.length === 0) {
    return (
      <div data-testid="mcp-tokens-empty" className="rounded-lg p-4 text-sm"
        style={{ ...card, color: 'var(--text-secondary)' }}>
        No MCP tokens yet. Create one above to connect a coding agent.
      </div>
    )
  }
  return <TokenTable tokens={list.tokens} now={list.at} owners={owners}
    revokingIds={revokingIds} onRevoke={onRevoke} />
}

export function MCPTokensPage() {
  const list = useTokenList()
  const owners = useOwners()
  const [actionError, setActionError] = useState(null)
  const { revokingIds, revoke, dialog } = useRevoke(list.replace, setActionError)
  const { created, creating, formError, create, dismiss } = useCreate(list.load)
  return (
    <div data-testid="mcp-tokens-page">
      <Explainer />
      <AgentsBanner />
      <SecretNotice created={created} onDismiss={dismiss} />
      <div className="rounded-lg p-4 mb-6" style={card}>
        <h2 className="text-sm font-semibold mb-3" style={{ color: 'var(--text-primary)' }}>
          Create MCP token
        </h2>
        <TokenForm onCreate={create} creating={creating} error={formError}
          owners={owners} />
      </div>
      {actionError && (
        <div data-testid="mcp-tokens-action-error" role="alert"
          className="text-sm p-3 rounded mb-4" style={errorStyle}>
          {actionError}
        </div>
      )}
      <TokenList list={list} owners={owners.owners} revokingIds={revokingIds}
        onRevoke={revoke} />
      {dialog}
    </div>
  )
}
