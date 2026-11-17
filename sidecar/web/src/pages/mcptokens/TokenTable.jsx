// MCP v2: the token list. Revoked and expired tokens stay listed, marked as
// such, and cannot be revoked again. Each row names who the token acts for:
// its agent principal or its owner. A retired agent's token is refused by
// the server, and a frozen agent's gets agent_frozen, so the row says so.
import { revocable, tokenStatus } from './tokenApi'
import { formatTime } from '../../lib/time'

const STATUS_COLORS = {
  active: 'var(--accent)',
  revoked: '#ef4444',
  expired: 'var(--text-secondary)',
  'agent retired': '#ef4444',
  'agent frozen': '#f59e0b',
}

const HEADERS = ['Name', 'Kind', 'Principal', 'Owner', 'Scopes', 'Databases',
  'Status', 'Expires', 'Last used', '']

function when(ts) {
  return formatTime(ts)
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

function ownerLabel(token, owners) {
  if (token.owner_user_id == null) return '-'
  const owner = (owners || []).find(u => u.id === token.owner_user_id)
  return owner ? owner.email : `user #${token.owner_user_id}`
}

function TokenRow({ token, now, owners, revoking, onRevoke }) {
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
      <td className={cell} data-testid={`mcp-token-principal-${id}`}>
        <code className="text-xs">{token.principal_id || '-'}</code>
      </td>
      <td className={cell} data-testid={`mcp-token-owner-${id}`}>
        {ownerLabel(token, owners)}
      </td>
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
        {revocable(status) && (
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
// owners are the users who may own a token, to name an owner by email.
export function TokenTable({ tokens, now, owners, revokingIds, onRevoke }) {
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
            <TokenRow key={t.id} token={t} now={now} owners={owners}
              revoking={revokingIds.has(t.id)} onRevoke={onRevoke} />
          ))}
        </tbody>
      </table>
    </div>
  )
}
