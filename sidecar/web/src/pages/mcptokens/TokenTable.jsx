// MCP v2: the token list. Revoked and expired tokens stay listed, marked as
// such, and only active tokens can be revoked.
import { tokenStatus } from './tokenApi'

const STATUS_COLORS = {
  active: 'var(--accent)',
  revoked: '#ef4444',
  expired: 'var(--text-secondary)',
}

const HEADERS = ['Name', 'Kind', 'Scopes', 'Databases', 'Status', 'Expires',
  'Last used', '']

function when(ts) {
  return new Date(ts).toLocaleString()
}

function lifecycle(token, status) {
  if (status === 'revoked') {
    return `Revoked ${when(token.revoked_at)}`
      + (token.revoked_by ? ` by ${token.revoked_by}` : '')
  }
  return `${status === 'expired' ? 'Expired' : 'Expires'} ${when(token.expires_at)}`
}

function databasesLabel(databases) {
  if (!databases?.length) return ''
  return databases.includes('*') ? 'All databases' : databases.join(', ')
}

function TokenRow({ token, now, revoking, onRevoke }) {
  const status = tokenStatus(token, now)
  const id = token.id
  const cell = 'px-4 py-2'
  return (
    <tr data-testid={`mcp-token-row-${id}`} style={{ borderBottom: '1px solid var(--border)' }}>
      <td className={cell} style={{ color: 'var(--text-primary)' }}>
        <div>{token.name}</div>
        <code className="text-xs" style={{ color: 'var(--text-secondary)' }}
          title={`Created by ${token.created_by}`}>
          {token.prefix}…
        </code>
      </td>
      <td className={cell}>{token.kind}</td>
      <td className={cell} data-testid={`mcp-token-scopes-${id}`}>
        {(token.scopes || []).join(', ')}
      </td>
      <td className={cell} data-testid={`mcp-token-databases-${id}`}>
        {databasesLabel(token.databases)}
      </td>
      <td className={cell}>
        <span data-testid={`mcp-token-status-${id}`}
          style={{ color: STATUS_COLORS[status] }}>{status}</span>
      </td>
      <td className={cell} style={{ color: 'var(--text-secondary)' }}>
        {lifecycle(token, status)}
      </td>
      <td className={cell} data-testid={`mcp-token-last-used-${id}`}
        style={{ color: 'var(--text-secondary)' }}>
        {token.last_used_at ? when(token.last_used_at) : 'Never'}
      </td>
      <td className={`${cell} text-right`}>
        {status === 'active' && (
          <button type="button" data-testid={`mcp-token-revoke-${id}`}
            disabled={revoking} onClick={() => onRevoke(token)}
            className="px-2 py-1 rounded text-xs"
            style={{ color: '#ef4444', opacity: revoking ? 0.6 : 1 }}>
            {revoking ? 'Revoking...' : 'Revoke'}
          </button>
        )}
      </td>
    </tr>
  )
}

// now is when the list was fetched: a token is active or expired as of then.
export function TokenTable({ tokens, now, revokingIds, onRevoke }) {
  return (
    <div className="rounded-lg overflow-x-auto"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      <table className="w-full text-sm" data-testid="mcp-tokens-table">
        <thead>
          <tr style={{ borderBottom: '1px solid var(--border)' }}>
            {HEADERS.map(h => (
              <th key={h} className="text-left px-4 py-2 text-xs"
                style={{ color: 'var(--text-secondary)' }}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody style={{ color: 'var(--text-primary)' }}>
          {tokens.map(t => (
            <TokenRow key={t.id} token={t} now={now}
              revoking={revokingIds.has(t.id)} onRevoke={onRevoke} />
          ))}
        </tbody>
      </table>
    </div>
  )
}
