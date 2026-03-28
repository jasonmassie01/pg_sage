import { useState } from 'react'

const SSL_MODES = [
  'disable', 'allow', 'prefer', 'require',
  'verify-ca', 'verify-full',
]
const TRUST_LEVELS = ['observation', 'advisory', 'autonomous']
const EXEC_MODES = ['auto', 'approval', 'manual']

export function DatabaseForm({ db, onClose, onError }) {
  const isEdit = !!db
  const [form, setForm] = useState({
    name: db?.name || '',
    host: db?.host || '',
    port: db?.port || 5432,
    database_name: db?.database_name || '',
    username: db?.username || '',
    password: '',
    sslmode: db?.sslmode || 'require',
    trust_level: db?.trust_level || 'observation',
    execution_mode: db?.execution_mode || 'approval',
  })
  const [saving, setSaving] = useState(false)
  const [testResult, setTestResult] = useState(null)
  const [testing, setTesting] = useState(false)

  function set(field) {
    return e => setForm(f => ({
      ...f,
      [field]: field === 'port'
        ? parseInt(e.target.value, 10) || 0
        : e.target.value,
    }))
  }

  async function handleTest() {
    if (!db?.id) return
    setTesting(true)
    setTestResult(null)
    try {
      const res = await fetch(
        `/api/v1/databases/managed/${db.id}/test`,
        { method: 'POST', credentials: 'include' })
      const data = await res.json()
      setTestResult(data)
    } catch (err) {
      setTestResult({ status: 'error', error: err.message })
    } finally {
      setTesting(false)
    }
  }

  async function handleSubmit(e) {
    e.preventDefault()
    setSaving(true)
    try {
      const url = isEdit
        ? `/api/v1/databases/managed/${db.id}`
        : '/api/v1/databases/managed'
      const method = isEdit ? 'PUT' : 'POST'
      const res = await fetch(url, {
        method,
        headers: { 'Content-Type': 'application/json' },
        credentials: 'include',
        body: JSON.stringify(form),
      })
      if (!res.ok) {
        const data = await res.json()
        throw new Error(data.error || 'Failed to save')
      }
      onClose()
    } catch (err) {
      onError(err.message)
    } finally {
      setSaving(false)
    }
  }

  const inputStyle = {
    background: 'var(--bg-main)',
    border: '1px solid var(--border)',
    color: 'var(--text-primary)',
  }

  return (
    <div className="rounded-lg p-4 mb-4"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <h2 className="text-sm font-semibold mb-3"
        style={{ color: 'var(--text-primary)' }}>
        {isEdit ? `Edit ${db.name}` : 'Add Database'}
      </h2>
      <form onSubmit={handleSubmit}>
        <div className="grid grid-cols-3 gap-3 mb-3">
          <Field label="Name" value={form.name}
            onChange={set('name')} style={inputStyle} required />
          <Field label="Host" value={form.host}
            onChange={set('host')} style={inputStyle} required />
          <Field label="Port" type="number" value={form.port}
            onChange={set('port')} style={inputStyle} required />
          <Field label="Database" value={form.database_name}
            onChange={set('database_name')} style={inputStyle}
            required />
          <Field label="Username" value={form.username}
            onChange={set('username')} style={inputStyle} required />
          <Field label="Password" type="password"
            value={form.password} onChange={set('password')}
            style={inputStyle}
            placeholder={isEdit ? '(unchanged)' : ''}
            required={!isEdit} />
          <SelectField label="SSL Mode" value={form.sslmode}
            onChange={set('sslmode')} options={SSL_MODES}
            style={inputStyle} />
          <SelectField label="Trust Level" value={form.trust_level}
            onChange={set('trust_level')} options={TRUST_LEVELS}
            style={inputStyle} />
          <SelectField label="Execution Mode"
            value={form.execution_mode}
            onChange={set('execution_mode')} options={EXEC_MODES}
            style={inputStyle} />
        </div>

        {testResult && (
          <TestResultBanner result={testResult} />
        )}

        <div className="flex gap-2 mt-3">
          <button type="submit" disabled={saving}
            className="px-4 py-1.5 rounded text-sm font-medium"
            style={{ background: 'var(--accent)', color: '#fff' }}>
            {saving ? 'Saving...' : 'Save'}
          </button>
          {isEdit && (
            <button type="button" onClick={handleTest}
              disabled={testing}
              className="px-4 py-1.5 rounded text-sm font-medium"
              style={{
                background: 'var(--bg-main)',
                color: 'var(--text-primary)',
                border: '1px solid var(--border)',
              }}>
              {testing ? 'Testing...' : 'Test Connection'}
            </button>
          )}
          <button type="button" onClick={onClose}
            className="px-4 py-1.5 rounded text-sm"
            style={{ color: 'var(--text-secondary)' }}>
            Cancel
          </button>
        </div>
      </form>
    </div>
  )
}

function Field({
  label, type = 'text', value, onChange, style,
  required, placeholder,
}) {
  return (
    <div>
      <label className="block text-xs mb-1"
        style={{ color: 'var(--text-secondary)' }}>{label}</label>
      <input type={type} value={value} onChange={onChange}
        required={required} placeholder={placeholder}
        className="w-full px-3 py-1.5 rounded text-sm"
        style={style} />
    </div>
  )
}

function SelectField({ label, value, onChange, options, style }) {
  return (
    <div>
      <label className="block text-xs mb-1"
        style={{ color: 'var(--text-secondary)' }}>{label}</label>
      <select value={value} onChange={onChange}
        className="w-full px-3 py-1.5 rounded text-sm"
        style={style}>
        {options.map(o => (
          <option key={o} value={o}>{o}</option>
        ))}
      </select>
    </div>
  )
}

function TestResultBanner({ result }) {
  const isOk = result.status === 'ok'
  return (
    <div className="text-xs p-2 rounded"
      style={{
        background: isOk
          ? 'rgba(16,185,129,0.1)' : 'rgba(239,68,68,0.1)',
        color: isOk ? '#10b981' : '#ef4444',
        border: `1px solid ${isOk
          ? 'rgba(16,185,129,0.3)' : 'rgba(239,68,68,0.3)'}`,
      }}>
      {isOk
        ? `Connected - ${result.pg_version || 'OK'}`
          + (result.extensions?.length
            ? ` | Extensions: ${result.extensions.join(', ')}`
            : '')
        : `Error: ${result.error}`}
    </div>
  )
}
