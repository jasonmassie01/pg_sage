import { useState, useEffect, useCallback } from 'react'

export function UsersPage() {
  const [users, setUsers] = useState([])
  const [error, setError] = useState(null)
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [role, setRole] = useState('viewer')
  const [creating, setCreating] = useState(false)

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
        body: JSON.stringify({ email, password, role }),
      })
      if (!res.ok) {
        const data = await res.json()
        throw new Error(data.error || 'Failed to create user')
      }
      setEmail('')
      setPassword('')
      setRole('viewer')
      fetchUsers()
    } catch (err) {
      setError(err.message)
    } finally {
      setCreating(false)
    }
  }

  async function handleDelete(id, userEmail) {
    if (!confirm(`Delete user ${userEmail}?`)) return
    try {
      const res = await fetch(`/api/v1/users/${id}`, {
        method: 'DELETE',
        credentials: 'include',
      })
      if (!res.ok) throw new Error('Failed to delete user')
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
      if (!res.ok) throw new Error('Failed to update role')
      fetchUsers()
    } catch (err) {
      setError(err.message)
    }
  }

  const inputStyle = {
    background: 'var(--bg-main)',
    border: '1px solid var(--border)',
    color: 'var(--text-primary)',
  }

  return (
    <div>
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

      <div className="rounded-lg p-4 mb-6"
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
              onChange={e => setEmail(e.target.value)}
              className="px-3 py-1.5 rounded text-sm"
              style={inputStyle} />
          </div>
          <div>
            <label className="block text-xs mb-1"
              style={{ color: 'var(--text-secondary)' }}>
              Password
            </label>
            <input type="password" value={password} required
              onChange={e => setPassword(e.target.value)}
              className="px-3 py-1.5 rounded text-sm"
              style={inputStyle} />
          </div>
          <div>
            <label className="block text-xs mb-1"
              style={{ color: 'var(--text-secondary)' }}>
              Role
            </label>
            <select value={role}
              onChange={e => setRole(e.target.value)}
              className="px-3 py-1.5 rounded text-sm"
              style={inputStyle}>
              <option value="viewer">viewer</option>
              <option value="operator">operator</option>
              <option value="admin">admin</option>
            </select>
          </div>
          <button type="submit" disabled={creating}
            className="px-4 py-1.5 rounded text-sm font-medium"
            style={{
              background: 'var(--accent)',
              color: '#fff',
            }}>
            {creating ? 'Adding...' : 'Add User'}
          </button>
        </form>
      </div>

      <div className="rounded-lg overflow-hidden"
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        <table className="w-full text-sm">
          <thead>
            <tr style={{ borderBottom: '1px solid var(--border)' }}>
              {['Email', 'Role', 'Created', 'Last Login', ''].map(
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
            {users.map(u => (
              <tr key={u.id}
                style={{
                  borderBottom: '1px solid var(--border)',
                }}>
                <td className="px-4 py-2"
                  style={{ color: 'var(--text-primary)' }}>
                  {u.email}
                </td>
                <td className="px-4 py-2">
                  <select value={u.role}
                    onChange={e =>
                      handleRoleChange(u.id, e.target.value)
                    }
                    className="px-2 py-1 rounded text-xs"
                    style={inputStyle}>
                    <option value="viewer">viewer</option>
                    <option value="operator">operator</option>
                    <option value="admin">admin</option>
                  </select>
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
                <td className="px-4 py-2 text-right">
                  <button
                    onClick={() => handleDelete(u.id, u.email)}
                    className="px-2 py-1 rounded text-xs"
                    style={{ color: '#ef4444' }}>
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
