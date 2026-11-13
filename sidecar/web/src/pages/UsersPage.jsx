import { Fragment, useState, useEffect, useCallback } from 'react'
import { SSOGrantNotice, UserSSOCell } from './users/UserSSOControls'
import { grantLinkURL, issueSSOLinkGrant, unlinkUserSSO } from './users/ssoApi'
import { DisabledBadge, UserStatusButton } from './users/UserStatusControls'
import { deleteUser, setUserDisabled } from './users/userAdminApi'
import { useConfirm } from '../hooks/useConfirm'
import { useConfirmTarget } from '../hooks/useConfirmTarget'
import {
  ResetPasswordButton, ResetPasswordDialog, TempPasswordNotice,
} from './users/UserPasswordReset'
import { resetUserPassword } from './users/passwordApi'
import { DatabaseGrantsButton, UserDatabaseGrants } from './users/UserDatabaseGrants'

export function UsersPage({ currentUser, authMode = currentUser?.auth_mode }) {
  const platformManaged = authMode === 'databricks_app'
  const [users, setUsers] = useState([])
  const [error, setError] = useState(null)
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [role, setRole] = useState('viewer')
  const [creating, setCreating] = useState(false)
  const [ssoOnly, setSSOOnly] = useState(false)
  const [grant, setGrant] = useState(null)
  const { askConfirm, dialog } = useConfirm()
  const [reset, setReset] = useState(null)
  const [grantsOpen, setGrantsOpen] = useState(null)
  const resetConfirm = useConfirmTarget({
    perform: user => {
      setError(null)
      setReset(null)
      return resetUserPassword(user.id)
    },
    onDone: (user, data) => setReset({ email: user.email, password: data.temporary_password }),
  })

  const fetchUsers = useCallback(async () => {
    try {
      const res = await fetch('/api/v1/users', {
        credentials: 'include',
      })
      if (!res.ok) throw new Error('Failed to load users')
      const data = await res.json()
      setUsers(data.users || [])
    } catch (err) {
      setError(err.message)
    }
  }, [])

  useEffect(() => { fetchUsers() }, [fetchUsers])

  async function handleCreate(e) {
    e.preventDefault()
    setCreating(true)
    setError(null)
    try {
      const res = await fetch('/api/v1/users', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify(ssoOnly
          ? { email, role, sso_only: true }
          : { email, password, role }),
      })
      if (!res.ok) {
        const data = await res.json()
        throw new Error(data.error || 'Failed to create user')
      }
      setEmail('')
      setPassword('')
      setRole('viewer')
      setSSOOnly(false)
      fetchUsers()
    } catch (err) {
      setError(err.message)
    } finally {
      setCreating(false)
    }
  }

  async function handleDelete(id, userEmail) {
    const ok = await askConfirm({
      title: `Delete user ${userEmail}`, action: 'Delete',
      consequence: 'Deleting cannot be undone. Deactivating instead keeps their '
        + 'history attributable and can be undone.',
    })
    if (!ok) return
    setError(null)
    try {
      await deleteUser(id)
      fetchUsers()
    } catch (err) {
      setError(err.message)
    }
  }

  async function handleStatus(user) {
    const disable = !user.disabled
    if (disable && !await askConfirm({
      title: `Deactivate ${user.email}`, action: 'Deactivate',
      consequence: 'They are signed out everywhere and their MCP tokens are revoked.',
    })) return
    setError(null)
    try {
      await setUserDisabled(user.id, disable)
      fetchUsers()
    } catch (err) {
      setError(err.message)
    }
  }

  async function handleRoleChange(id, newRole) {
    try {
      const res = await fetch(`/api/v1/users/${id}/role`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify({ role: newRole }),
      })
      if (!res.ok) {
        const data = await res.json().catch(() => ({}))
        throw new Error(data.error || 'Failed to update role')
      }
      fetchUsers()
    } catch (err) {
      setError(err.message)
    }
  }

  async function handleUnlink(user) {
    const ok = await askConfirm({
      title: `Unlink SSO from ${user.email}`, action: 'Unlink',
      consequence: 'Their sessions end.',
    })
    if (!ok) return
    setError(null)
    try {
      await unlinkUserSSO(user.id)
      fetchUsers()
    } catch (err) {
      setError(err.message)
    }
  }

  async function handleGrant(user) {
    setError(null)
    setGrant(null)
    try {
      const data = await issueSSOLinkGrant(user.id)
      setGrant({ email: user.email, url: grantLinkURL(data.token),
        expiresAt: data.expires_at })
    } catch (err) {
      setError(err.message)
    }
  }

  const inputStyle = {
    background: 'var(--bg-main)',
    border: '1px solid var(--border)',
    color: 'var(--text-primary)',
  }
  // Only enabled admins count: a deactivated admin holds no admin power.
  const adminCount = users.filter(u => u.role === 'admin' && !u.disabled).length

  return (
    <div>
      {dialog}
      <ResetPasswordDialog reset={resetConfirm} />
      {error && (
        <div className="text-sm p-3 rounded mb-4"
          style={{
            background: 'rgba(239,68,68,0.1)',
            color: '#ef4444',
            border: '1px solid rgba(239,68,68,0.3)',
          }}>
          {error}
        </div>
      )}

      {!platformManaged && <SSOGrantNotice grant={grant} onDismiss={() => setGrant(null)} />}
      {!platformManaged && <TempPasswordNotice reset={reset} onDismiss={() => setReset(null)} />}

      {platformManaged && (
        <p className="text-sm mb-4" style={{ color: 'var(--text-secondary)' }}>
          Access is managed in Databricks. App roles below control pg_sage permissions.
        </p>
      )}

      {!platformManaged && <div className="rounded-lg p-4 mb-6"
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        <h2 className="text-sm font-semibold mb-3"
          style={{ color: 'var(--text-primary)' }}>
          Add User
        </h2>
        <form onSubmit={handleCreate}
          className="flex items-end gap-3 flex-wrap">
          <div>
            <label className="block text-xs mb-1"
              style={{ color: 'var(--text-secondary)' }}>
              Email
            </label>
            <input type="email" value={email} required
              data-testid="add-user-email"
              onChange={e => setEmail(e.target.value)}
              className="px-3 py-1.5 rounded text-sm"
              style={inputStyle} />
          </div>
          <div>
            <label className="block text-xs mb-1"
              style={{ color: 'var(--text-secondary)' }}>
              Password
            </label>
            <input type="password" value={password} required={!ssoOnly}
              disabled={ssoOnly}
              data-testid="add-user-password"
              onChange={e => setPassword(e.target.value)}
              className="px-3 py-1.5 rounded text-sm"
              style={{ ...inputStyle, opacity: ssoOnly ? 0.5 : 1 }} />
          </div>
          <label className="flex items-center gap-1 text-xs pb-2"
            style={{ color: 'var(--text-secondary)' }}>
            <input type="checkbox" checked={ssoOnly}
              data-testid="add-user-sso-only"
              onChange={e => {
                setSSOOnly(e.target.checked)
                setPassword('')
              }} />
            SSO only (no password)
          </label>
          <div>
            <label className="block text-xs mb-1"
              style={{ color: 'var(--text-secondary)' }}>
              Role
            </label>
            <select value={role}
              data-testid="add-user-role"
              onChange={e => setRole(e.target.value)}
              className="px-3 py-1.5 rounded text-sm"
              style={inputStyle}>
              <option value="viewer">viewer</option>
              <option value="operator">operator</option>
              <option value="admin">admin</option>
            </select>
          </div>
          <button type="submit" disabled={creating}
            data-testid="add-user-submit"
            className="px-4 py-1.5 rounded text-sm font-medium"
            style={{
              background: 'var(--accent)',
              color: '#fff',
            }}>
            {creating ? 'Adding...' : 'Add User'}
          </button>
        </form>
      </div>}

      <div className="rounded-lg overflow-hidden"
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        <table className="w-full text-sm" data-testid="users-table">
          <thead>
            <tr style={{ borderBottom: '1px solid var(--border)' }}>
              {['Email', 'Role', platformManaged ? 'Sign-in' : 'SSO', 'Created', 'Last Login',
                ...(!platformManaged ? [''] : [])].map(
                h => (
                  <th key={h} className="text-left px-4 py-2 text-xs"
                    style={{ color: 'var(--text-secondary)' }}>
                    {h}
                  </th>
                ),
              )}
            </tr>
          </thead>
          <tbody>
            {users.map(u => {
              const isCurrentUser = currentUser?.id === u.id
              const isLastAdmin =
                u.role === 'admin' && !u.disabled && adminCount <= 1
              const roleLocked = isCurrentUser || isLastAdmin
              const lockTitle = isCurrentUser
                ? 'You cannot change your own admin role'
                : isLastAdmin
                  ? 'At least one admin must remain'
                  : undefined
              return (
              <Fragment key={u.id}>
              <tr
                style={{
                  borderBottom: '1px solid var(--border)',
                }}>
                <td className="px-4 py-2"
                  style={{ color: 'var(--text-primary)' }}>
                  {u.email}
                  <DisabledBadge user={u} />
                </td>
                <td className="px-4 py-2">
                  <select value={u.role}
                    onChange={e =>
                      handleRoleChange(u.id, e.target.value)
                    }
                    disabled={roleLocked}
                    title={lockTitle}
                    data-testid="user-role-select"
                    className="px-2 py-1 rounded text-xs"
                    style={{
                      ...inputStyle,
                      opacity: roleLocked ? 0.6 : 1,
                    }}>
                    <option value="viewer">viewer</option>
                    <option value="operator">operator</option>
                    <option value="admin">admin</option>
                  </select>
                </td>
                <td className="px-4 py-2">
                  {platformManaged ? 'Databricks' : <UserSSOCell user={u} onUnlink={handleUnlink}
                    onGrant={handleGrant} />}
                </td>
                <td className="px-4 py-2"
                  style={{ color: 'var(--text-secondary)' }}>
                  {new Date(u.created_at).toLocaleDateString()}
                </td>
                <td className="px-4 py-2"
                  style={{ color: 'var(--text-secondary)' }}>
                  {u.last_login
                    ? new Date(u.last_login).toLocaleString()
                    : 'Never'}
                </td>
                {!platformManaged && <td className="px-4 py-2 text-right">
                  <UserStatusButton user={u}
                    locked={!u.disabled && (isCurrentUser || isLastAdmin)}
                    lockTitle={isCurrentUser
                      ? 'You cannot deactivate your own account'
                      : 'At least one enabled admin must remain'}
                    onToggle={handleStatus} />
                  <ResetPasswordButton user={u} isCurrentUser={isCurrentUser}
                    onReset={resetConfirm.open} />
                  <DatabaseGrantsButton user={u} open={grantsOpen === u.id}
                    onToggle={id => setGrantsOpen(grantsOpen === id ? null : id)} />
                  <button
                    onClick={() => handleDelete(u.id, u.email)}
                    disabled={isCurrentUser || isLastAdmin}
                    title={isCurrentUser
                      ? 'You cannot delete your own account'
                      : isLastAdmin
                        ? 'At least one admin must remain'
                        : undefined}
                    data-testid="user-delete-button"
                    className="px-2 py-1 rounded text-xs"
                    style={{
                      color: '#ef4444',
                      opacity: (isCurrentUser || isLastAdmin) ? 0.6 : 1,
                    }}>
                    Delete
                  </button>
                </td>}
              </tr>
              {grantsOpen === u.id && u.role !== 'admin' && <tr>
                <td colSpan={6}><UserDatabaseGrants user={u} /></td>
              </tr>}
              </Fragment>
              )
            })}
          </tbody>
        </table>
      </div>
    </div>
  )
}
