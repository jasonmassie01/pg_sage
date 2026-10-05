// MCP v2: admins create and revoke the scoped MCP API tokens coding agents
// (Claude Code, Cursor) use to reach pg_sage. Admin only: App.jsx gates the
// route and Layout hides the nav link from other roles.
import { useCallback, useEffect, useState } from 'react'
import { ExternalAgentRequests } from './mcptokens/ExternalAgentRequests'
import { SecretNotice } from './mcptokens/SecretNotice'
import { TokenForm } from './mcptokens/TokenForm'
import { TokenTable } from './mcptokens/TokenTable'
import { createToken, listTokens, revokeToken } from './mcptokens/tokenApi'

const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
const errorStyle = {
  background: 'rgba(239,68,68,0.1)',
  color: '#ef4444',
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
        A token lets a coding agent such as Claude Code or Cursor read pg_sage&apos;s
        findings and propose fixes over MCP. Agent tokens can read and propose but
        never approve; an operator token acts for its owner and may approve.
        Every proposal still goes through pg_sage&apos;s trust gates. See docs/mcp.md
        for setup and the tools each scope unlocks.
      </p>
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
  const mark = (id, on) => setRevokingIds(prev => {
    const next = new Set(prev)
    if (on) next.add(id)
    else next.delete(id)
    return next
  })
  async function revoke(token) {
    if (revokingIds.has(token.id)) return
    if (!confirm(`Revoke MCP token "${token.name}"? Agents using it lose access now.`)) {
      return
    }
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
  return { revokingIds, revoke }
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

function TokenList({ list, revokingIds, onRevoke }) {
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
  return <TokenTable tokens={list.tokens} now={list.at} revokingIds={revokingIds}
    onRevoke={onRevoke} />
}

export function MCPTokensPage() {
  const list = useTokenList()
  const [actionError, setActionError] = useState(null)
  const { revokingIds, revoke } = useRevoke(list.replace, setActionError)
  const { created, creating, formError, create, dismiss } = useCreate(list.load)
  return (
    <div data-testid="mcp-tokens-page">
      <Explainer />
      <SecretNotice created={created} onDismiss={dismiss} />
      <div className="rounded-lg p-4 mb-6" style={card}>
        <h2 className="text-sm font-semibold mb-3" style={{ color: 'var(--text-primary)' }}>
          Create MCP token
        </h2>
        <TokenForm onCreate={create} creating={creating} error={formError} />
      </div>
      {actionError && (
        <div data-testid="mcp-tokens-action-error" role="alert"
          className="text-sm p-3 rounded mb-4" style={errorStyle}>
          {actionError}
        </div>
      )}
      <TokenList list={list} revokingIds={revokingIds} onRevoke={revoke} />
      <ExternalAgentRequests />
    </div>
  )
}
