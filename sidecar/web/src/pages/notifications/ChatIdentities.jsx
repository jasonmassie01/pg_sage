// Chat identities (alerting-10): an admin links a Slack or Telegram user to
// a pg_sage user, who can then approve from chat with that account's
// current role. The routes are admin-only; anyone else gets a 403 and the
// panel is not shown at all.
import { useCallback, useEffect, useState } from 'react'
import { ConfirmDialog } from '../../components/ConfirmDialog'
import { ErrorBanner } from '../../components/ErrorBanner'
import { useConfirmTarget } from '../../hooks/useConfirmTarget'
import { cardStyle, inputStyle, FormField } from './shared'
import { failureMessage } from './channelConfig'

const IDENTITIES_URL = '/api/v1/chatops/identities'

async function loadUsers() {
  try {
    const res = await fetch('/api/v1/users', { credentials: 'include' })
    if (!res.ok) return []
    const data = await res.json()
    return data.users || []
  } catch (err) {
    // The picker falls back to a numeric user id.
    console.warn('loading users for chat identities failed:', err)
    return []
  }
}

function UserPicker({ users, value, onChange }) {
  if (!users.length) {
    return (
      <input type="number" min="1" value={value} required aria-label="pg_sage user id"
        data-testid="chat-identity-user" onChange={e => onChange(e.target.value)}
        className="px-3 py-1.5 rounded text-sm w-28" style={inputStyle} />
    )
  }
  return (
    <select value={value} required aria-label="pg_sage user" data-testid="chat-identity-user"
      onChange={e => onChange(e.target.value)}
      className="px-3 py-1.5 rounded text-sm" style={inputStyle}>
      <option value="">Choose a user</option>
      {users.map(u => (
        <option key={u.id} value={String(u.id)}>{u.email} ({u.role})</option>
      ))}
    </select>
  )
}

// linkIdentity posts one mapping and returns why it failed, or null.
async function linkIdentity(body) {
  try {
    const res = await fetch(IDENTITIES_URL, {
      method: 'POST', credentials: 'include',
      headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    })
    return res.ok ? null : await failureMessage(res, 'Failed to link chat user')
  } catch (err) {
    return `Failed to link chat user: ${err.message}`
  }
}

// deleteIdentity removes one mapping or throws an Error saying why.
async function deleteIdentity(id) {
  let res
  try {
    res = await fetch(`${IDENTITIES_URL}/${id}`, { method: 'DELETE', credentials: 'include' })
  } catch (err) {
    throw new Error(`Failed to unlink chat user: ${err.message}`)
  }
  if (!res.ok) throw new Error(await failureMessage(res, 'Failed to unlink chat user'))
}

// UnlinkDialog asks before a chat user is unlinked (dashboard-10b); a
// failure stays in the open dialog and the row stays listed.
function UnlinkDialog({ unlink }) {
  const it = unlink.target
  return (
    <ConfirmDialog open={it !== null}
      title={it ? `Unlink chat user ${it.external_user_id || '(no id)'}` : ''}
      action="Unlink" consequence="They can no longer approve from chat."
      busy={unlink.busy} error={unlink.error?.message} onConfirm={unlink.accept}
      onCancel={unlink.cancel} />
  )
}

function ProviderSelect({ value, onChange }) {
  return (
    <FormField label="Provider">
      <select value={value} aria-label="Provider" data-testid="chat-identity-provider"
        onChange={e => onChange(e.target.value)}
        className="px-3 py-1.5 rounded text-sm" style={inputStyle}>
        <option value="slack">Slack</option>
        <option value="telegram">Telegram</option>
      </select>
    </FormField>
  )
}

function LinkForm({ users, onLinked, onError }) {
  const [provider, setProvider] = useState('slack')
  const [teamId, setTeamId] = useState('')
  const [externalId, setExternalId] = useState('')
  const [userId, setUserId] = useState('')

  async function submit(e) {
    e.preventDefault()
    const body = { provider, external_user_id: externalId.trim(), user_id: Number(userId) }
    if (provider === 'slack') body.team_id = teamId.trim()
    const failure = await linkIdentity(body)
    if (failure) {
      onError(failure)
      return
    }
    setExternalId('')
    onLinked()
  }

  return (
    <form onSubmit={submit} className="flex gap-3 flex-wrap items-end mb-3">
      <ProviderSelect value={provider} onChange={setProvider} />
      {provider === 'slack' && (
        <FormField label="Team ID">
          <input value={teamId} aria-label="Team ID" data-testid="chat-identity-team-id"
            onChange={e => setTeamId(e.target.value)} placeholder="T0123"
            className="px-3 py-1.5 rounded text-sm w-28" style={inputStyle} />
        </FormField>
      )}
      <FormField label={provider === 'slack' ? 'Slack user ID' : 'Telegram user ID (numeric)'}>
        <input value={externalId} required aria-label="Chat user ID"
          data-testid="chat-identity-external-user-id"
          onChange={e => setExternalId(e.target.value)}
          className="px-3 py-1.5 rounded text-sm" style={inputStyle} />
      </FormField>
      <FormField label="pg_sage user">
        <UserPicker users={users} value={userId} onChange={setUserId} />
      </FormField>
      <button type="submit" data-testid="chat-identity-link"
        className="px-4 py-1.5 rounded text-sm font-medium"
        style={{ background: 'var(--accent)', color: '#fff' }}>
        Link
      </button>
    </form>
  )
}

function IdentityTable({ items, onUnlink }) {
  if (!items.length) {
    return (
      <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
        No chat users linked yet.
      </p>
    )
  }
  return (
    <table className="w-full text-sm" data-testid="chat-identities-table">
      <thead>
        <tr>
          {['Provider', 'Team', 'Chat user', 'pg_sage user', ''].map(h => (
            <th key={h} className="text-left px-2 py-1 text-xs"
              style={{ color: 'var(--text-secondary)' }}>{h}</th>
          ))}
        </tr>
      </thead>
      <tbody>
        {items.map(it => (
          <tr key={it.id} data-testid="chat-identity-row"
            style={{ borderTop: '1px solid var(--border)', color: 'var(--text-primary)' }}>
            <td className="px-2 py-1">{it.provider}</td>
            <td className="px-2 py-1">{it.team_id || '-'}</td>
            <td className="px-2 py-1">{it.external_user_id}</td>
            <td className="px-2 py-1">{it.user_email || `user ${it.user_id}`}</td>
            <td className="px-2 py-1 text-right">
              <button type="button" onClick={() => onUnlink(it)}
                data-testid="chat-identity-unlink" className="text-xs px-2 py-1 rounded"
                style={{ color: 'var(--red)' }}>
                Unlink
              </button>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

// useIdentities loads the mappings; hidden is set when the caller is not
// an admin (401/403), so the panel is never shown to them.
function useIdentities() {
  const [items, setItems] = useState(null)
  const [hidden, setHidden] = useState(false)
  const [error, setError] = useState(null)
  const [users, setUsers] = useState([])

  const load = useCallback(async () => {
    try {
      const res = await fetch(IDENTITIES_URL, { credentials: 'include' })
      if (res.status === 401 || res.status === 403) {
        setHidden(true)
        return
      }
      if (!res.ok) throw new Error(`Failed to load chat identities (${res.status})`)
      const data = await res.json()
      setItems(data.items || [])
      setError(null)
    } catch (err) {
      setError(err.message)
    }
  }, [])

  useEffect(() => { load() }, [load])

  // Users are read only once the panel is shown, so a non-admin makes no
  // second admin-only request.
  const shown = items !== null
  useEffect(() => {
    if (shown) loadUsers().then(setUsers)
  }, [shown])

  const unlink = useConfirmTarget({ perform: it => deleteIdentity(it.id), onDone: load })

  return { items, hidden, error, setError, users, load, unlink }
}

export function ChatIdentities() {
  const { items, hidden, error, setError, users, load, unlink } = useIdentities()
  if (hidden || (items === null && !error)) return null
  return (
    <div data-testid="chat-identities" className="rounded-lg p-4 mt-6" style={cardStyle}>
      <h2 className="text-sm font-semibold mb-1" style={{ color: 'var(--text-primary)' }}>
        Chat identities
      </h2>
      <p className="text-xs mb-3" style={{ color: 'var(--text-secondary)' }}>
        Link a Slack or Telegram user to a pg_sage user. Only linked users can approve
        from chat, and only while their pg_sage role is operator or admin.
      </p>
      {error && <div className="mb-3"><ErrorBanner message={error} /></div>}
      <LinkForm users={users} onLinked={() => { setError(null); load() }}
        onError={setError} />
      <UnlinkDialog unlink={unlink} />
      {items && <IdentityTable items={items} onUnlink={unlink.open} />}
    </div>
  )
}
