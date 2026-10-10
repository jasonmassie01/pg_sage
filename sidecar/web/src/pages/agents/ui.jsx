/* eslint-disable react-refresh/only-export-components */
// Shared styles and small pieces of the Agents page.

export const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
export const errorStyle = {
  background: 'rgba(239,68,68,0.1)',
  color: '#ef4444',
  border: '1px solid rgba(239,68,68,0.3)',
}
export const inputStyle = {
  background: 'var(--bg-main)', border: '1px solid var(--border)',
  color: 'var(--text-primary)',
}
export const muted = { color: 'var(--text-secondary)' }

export function Section({ title, testId, children }) {
  return (
    <section data-testid={testId} className="rounded-lg p-4 mb-4" style={card}>
      <h3 className="text-sm font-semibold mb-3" style={{ color: 'var(--text-primary)' }}>
        {title}
      </h3>
      {children}
    </section>
  )
}

export function ErrorBox({ testId, error }) {
  if (!error) return null
  return (
    <div data-testid={testId} role="alert" className="text-sm p-3 rounded mb-3"
      style={errorStyle}>
      {error.message}
      {error.reason && <span className="ml-1">({error.reason})</span>}
      {error.fix && (
        <pre className="mt-2 text-xs whitespace-pre-wrap break-all">{error.fix}</pre>
      )}
    </div>
  )
}

export function Button({ testId, onClick, disabled, children, danger }) {
  return (
    <button type="button" data-testid={testId} onClick={onClick} disabled={disabled}
      className="px-3 py-1 rounded text-sm disabled:opacity-50"
      style={{ color: danger ? '#fff' : 'var(--accent)',
        background: danger ? '#dc2626' : 'transparent',
        border: danger ? 'none' : '1px solid var(--border)' }}>
      {children}
    </button>
  )
}

export function TextInput({ testId, value, onChange, placeholder, label }) {
  return (
    <input data-testid={testId} value={value} placeholder={placeholder}
      aria-label={label || placeholder}
      onChange={e => onChange(e.target.value)}
      className="px-2 py-1 rounded text-sm" style={inputStyle} />
  )
}

export function when(iso) {
  if (!iso) return ''
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString()
}
