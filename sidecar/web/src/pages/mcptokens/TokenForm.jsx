// MCP v2: the create-token form. An agent token can never hold "approve";
// an operator token needs an owner who is an operator or admin.
import { useEffect, useState } from 'react'
import { MAX_DAYS, MIN_DAYS, SCOPES, listOwners, requestBody } from './tokenApi'

const EMPTY_FORM = {
  name: '', kind: 'agent', scopes: ['read'], allDatabases: true,
  databaseNames: '', expires: '30', owner: '',
}

const inputStyle = {
  background: 'var(--bg-main)',
  border: '1px solid var(--border)',
  color: 'var(--text-primary)',
}

function Field({ label, children }) {
  return (
    <label className="block text-xs" style={{ color: 'var(--text-secondary)' }}>
      <span className="block mb-1">{label}</span>
      {children}
    </label>
  )
}

function ScopeFields({ form, onToggle }) {
  return (
    <fieldset className="flex items-center gap-3 text-xs"
      style={{ color: 'var(--text-secondary)' }}>
      <legend className="mb-1">Scopes</legend>
      {SCOPES.map(scope => {
        const locked = scope === 'approve' && form.kind !== 'operator'
        return (
          <label key={scope} className="flex items-center gap-1"
            title={locked ? 'Only operator tokens can approve actions' : undefined}
            style={{ opacity: locked ? 0.5 : 1 }}>
            <input type="checkbox" data-testid={`mcp-token-scope-${scope}`}
              checked={!locked && form.scopes.includes(scope)} disabled={locked}
              onChange={() => onToggle(scope)} />
            {scope}
          </label>
        )
      })}
    </fieldset>
  )
}

function DatabaseFields({ form, set }) {
  return (
    <div className="flex items-end gap-3">
      <label className="flex items-center gap-1 text-xs pb-2"
        style={{ color: 'var(--text-secondary)' }}>
        <input type="checkbox" data-testid="mcp-token-all-databases"
          checked={form.allDatabases}
          onChange={e => set({ allDatabases: e.target.checked })} />
        All databases
      </label>
      <Field label="Database names (comma separated)">
        <input type="text" data-testid="mcp-token-database-names"
          value={form.databaseNames} disabled={form.allDatabases}
          placeholder="orders, billing"
          onChange={e => set({ databaseNames: e.target.value })}
          className="px-3 py-1.5 rounded text-sm"
          style={{ ...inputStyle, opacity: form.allDatabases ? 0.5 : 1 }} />
      </Field>
    </div>
  )
}

function useOwners(needed) {
  const [state, setState] = useState({ owners: null, error: null })
  useEffect(() => {
    if (!needed || state.owners || state.error) return
    let live = true
    listOwners()
      .then(owners => { if (live) setState({ owners, error: null }) })
      .catch(err => { if (live) setState({ owners: null, error: err.message }) })
    return () => { live = false }
  }, [needed, state.owners, state.error])
  return state
}

function OwnerField({ form, set }) {
  const { owners, error } = useOwners(form.kind === 'operator')
  if (form.kind !== 'operator') return null
  const hint = error ? `Could not load users: ${error}`
    : owners && owners.length === 0
      ? 'No operator or admin users exist; add one on the Users page first.'
      : 'The token acts as this operator or admin user.'
  return (
    <div>
      <Field label="Owner">
        <select data-testid="mcp-token-owner" value={form.owner}
          onChange={e => set({ owner: e.target.value })}
          className="px-3 py-1.5 rounded text-sm" style={inputStyle}>
          <option value="">Choose an owner</option>
          {(owners || []).map(u => (
            <option key={u.id} value={String(u.id)}>{u.email} ({u.role})</option>
          ))}
        </select>
      </Field>
      <p data-testid="mcp-token-owner-hint" className="text-xs mt-1"
        style={{ color: 'var(--text-secondary)' }}>
        {hint}
      </p>
    </div>
  )
}

function useTokenForm() {
  const [form, setForm] = useState(EMPTY_FORM)
  const set = patch => setForm(prev => ({ ...prev, ...patch }))
  const setKind = kind => setForm(prev => kind === 'operator'
    ? { ...prev, kind }
    : { ...prev, kind, owner: '', scopes: prev.scopes.filter(s => s !== 'approve') })
  const toggleScope = scope => setForm(prev => ({
    ...prev,
    scopes: prev.scopes.includes(scope)
      ? prev.scopes.filter(s => s !== scope) : [...prev.scopes, scope],
  }))
  const reset = () => setForm(EMPTY_FORM)
  return { form, set, setKind, toggleScope, reset }
}

export function TokenForm({ onCreate, creating, error }) {
  const { form, set, setKind, toggleScope, reset } = useTokenForm()
  const body = requestBody(form)

  async function handleSubmit(e) {
    e.preventDefault()
    if (!body || creating) return
    if (await onCreate(body)) reset()
  }

  return (
    <form data-testid="mcp-token-form" onSubmit={handleSubmit}
      className="flex flex-col gap-3">
      <div className="flex items-end gap-3 flex-wrap">
        <Field label="Name">
          <input type="text" data-testid="mcp-token-name" value={form.name}
            placeholder="claude-code-laptop" maxLength={100}
            onChange={e => set({ name: e.target.value })}
            className="px-3 py-1.5 rounded text-sm" style={inputStyle} />
        </Field>
        <Field label="Kind">
          <select data-testid="mcp-token-kind" value={form.kind}
            onChange={e => setKind(e.target.value)}
            className="px-3 py-1.5 rounded text-sm" style={inputStyle}>
            <option value="agent">agent</option>
            <option value="operator">operator</option>
          </select>
        </Field>
        <Field label={`Expires in days (${MIN_DAYS}-${MAX_DAYS})`}>
          <input type="number" data-testid="mcp-token-expires" value={form.expires}
            min={MIN_DAYS} max={MAX_DAYS} step={1}
            onChange={e => set({ expires: e.target.value })}
            className="px-3 py-1.5 rounded text-sm w-24" style={inputStyle} />
        </Field>
        <OwnerField form={form} set={set} />
      </div>
      <ScopeFields form={form} onToggle={toggleScope} />
      <DatabaseFields form={form} set={set} />
      {error && (
        <div data-testid="mcp-token-form-error" role="alert" className="text-sm"
          style={{ color: '#ef4444' }}>
          {error}
        </div>
      )}
      <div>
        <button type="submit" data-testid="mcp-token-submit" disabled={!body || creating}
          className="px-4 py-1.5 rounded text-sm font-medium"
          style={{ background: 'var(--accent)', color: '#fff',
            opacity: !body || creating ? 0.6 : 1 }}>
          {creating ? 'Creating...' : 'Create token'}
        </button>
      </div>
    </form>
  )
}
