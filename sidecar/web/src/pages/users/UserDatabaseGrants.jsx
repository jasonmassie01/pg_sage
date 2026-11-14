import { useCallback, useEffect, useState } from 'react'
import { listDatabaseGrants, revokeDatabaseGrant, setDatabaseGrant } from './databaseGrantsApi'

// apiauth-08: an admin limits a user to some databases. A user with no
// grants keeps their role everywhere; with grants they are an operator
// only where granted (and never above their own role) and a viewer
// elsewhere. Admins are never limited. A grant the SSO group mapping wrote
// (apiauth-08d, source 'idp') is shown with its source and is read-only
// while the mapping is set; adding a grant on its database overrides it.

const inputStyle = {
  background: 'var(--bg-main)', color: 'var(--text-primary)',
  border: '1px solid var(--border)',
}

export function DatabaseGrantsButton({ user, open, onToggle }) {
  if (user.role === 'admin') return null
  return (
    <button onClick={() => onToggle(user.id)} data-testid="user-grants-button"
      className="px-2 py-1 rounded text-xs" style={{ color: 'var(--accent)' }}>
      {open ? 'Hide databases' : 'Databases'}
    </button>
  )
}

function GrantNotice({ enforced, scoped, role }) {
  const text = !enforced
    ? 'Grants are saved but not enforced: set api.database_grants: true in fleet mode.'
    : scoped
      ? `Limited to the databases below; a viewer on every other database.`
      : `No grants: ${role} on every database.`
  return (
    <p className="text-xs mb-2" data-testid="user-grants-notice"
      style={{ color: enforced ? 'var(--text-secondary)' : '#f59e0b' }}>
      {text}
    </p>
  )
}

function GrantSource({ idp, managed }) {
  const title = !idp ? 'Set by an administrator'
    : managed ? 'Set by the IdP group mapping at sign-in; change their IdP groups, ' +
      'or add a grant on this database to override it'
      : 'Set by an IdP group mapping that is no longer configured'
  return (
    <span data-testid="user-grant-source" title={title} className="px-1.5 rounded text-xs"
      style={{ color: 'var(--text-secondary)', border: '1px solid var(--border)' }}>
      {idp ? 'SSO' : 'manual'}
    </span>
  )
}

function GrantRole({ grant, idp, onRole }) {
  if (idp) {
    return (
      <span data-testid="user-grant-role-readonly" className="text-xs"
        style={{ color: 'var(--text-primary)' }}>{grant.role}</span>
    )
  }
  return (
    <select value={grant.role} data-testid="user-grant-role"
      onChange={e => onRole(grant.database, e.target.value)}
      className="px-2 py-0.5 rounded text-xs" style={inputStyle}>
      <option value="viewer">viewer</option>
      <option value="operator">operator</option>
    </select>
  )
}

function GrantRow({ grant, last, idpManaged, onRole, onRemove }) {
  const idp = grant.source === 'idp'
  return (
    <li className="flex items-center gap-2 py-1" data-testid="user-grant-row">
      <span className="font-mono text-xs" style={{ color: 'var(--text-primary)' }}>
        {grant.database}
      </span>
      <GrantRole grant={grant} idp={idp} onRole={onRole} />
      <GrantSource idp={idp} managed={idpManaged} />
      {!(idp && idpManaged) && <button onClick={() => onRemove(grant.database)}
        data-testid="user-grant-remove"
        title={last ? 'Removing the last grant returns the user to their role on ' +
          'every database' : undefined}
        className="px-2 py-0.5 rounded text-xs" style={{ color: '#ef4444' }}>
        Remove
      </button>}
    </li>
  )
}

function AddGrantForm({ onAdd }) {
  const [database, setDatabase] = useState('')
  const [role, setRole] = useState('operator')
  function submit(e) {
    e.preventDefault()
    if (database.trim() === '') return
    onAdd(database.trim(), role).then(added => { if (added) setDatabase('') })
  }
  return (
    <form onSubmit={submit} className="flex items-center gap-2 mt-2">
      <input value={database} placeholder="database name" data-testid="user-grant-database"
        onChange={e => setDatabase(e.target.value)}
        className="px-2 py-1 rounded text-xs" style={inputStyle} />
      <select value={role} onChange={e => setRole(e.target.value)}
        data-testid="user-grant-new-role" className="px-2 py-1 rounded text-xs"
        style={inputStyle}>
        <option value="viewer">viewer</option>
        <option value="operator">operator</option>
      </select>
      <button type="submit" data-testid="user-grant-add" className="px-3 py-1 rounded text-xs"
        style={{ background: 'var(--accent)', color: '#fff' }}>
        Grant
      </button>
    </form>
  )
}

export function UserDatabaseGrants({ user }) {
  const [state, setState] = useState(null)
  const [error, setError] = useState(null)
  const load = useCallback(() => listDatabaseGrants(user.id)
    .then(data => { setState(data); setError(null) })
    .catch(err => setError(err.message)), [user.id])
  useEffect(() => { load() }, [load])

  const change = (fn) => fn().then(() => load().then(() => true))
    .catch(err => { setError(err.message); return false })
  const grants = state?.grants || []
  return (
    <div className="px-4 py-3" data-testid="user-grants-panel"
      style={{ background: 'var(--bg-main)' }}>
      {state && <GrantNotice enforced={state.enforced} scoped={state.scoped}
        role={user.role} />}
      {error && <p className="text-xs mb-2" data-testid="user-grants-error"
        style={{ color: '#ef4444' }}>{error}</p>}
      <ul>
        {grants.map(g => (
          <GrantRow key={g.database} grant={g} last={grants.length === 1}
            idpManaged={Boolean(state?.idp_managed)}
            onRole={(db, role) => change(() => setDatabaseGrant(user.id, db, role))}
            onRemove={db => change(() => revokeDatabaseGrant(user.id, db))} />
        ))}
      </ul>
      <AddGrantForm onAdd={(db, role) => change(() => setDatabaseGrant(user.id, db, role))} />
    </div>
  )
}
