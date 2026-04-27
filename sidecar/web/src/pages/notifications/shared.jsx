/* eslint-disable react-refresh/only-export-components */
export const inputStyle = {
  background: 'var(--bg-main)',
  border: '1px solid var(--border)',
  color: 'var(--text-primary)',
}

export const cardStyle = {
  background: 'var(--bg-card)',
  border: '1px solid var(--border)',
}

export const EVENT_TYPES = [
  'action_executed',
  'action_failed',
  'approval_needed',
  'finding_critical',
]

export const SEVERITIES = ['info', 'warning', 'critical']

export function FormField({ label, children }) {
  return (
    <div>
      <label className="block text-xs mb-1"
        style={{ color: 'var(--text-secondary)' }}>
        {label}
      </label>
      {children}
    </div>
  )
}

export function ErrorBanner({ msg }) {
  return (
    <div className="text-sm p-3 rounded mb-4"
      style={{
        background: 'rgba(239,68,68,0.1)',
        color: '#ef4444',
        border: '1px solid rgba(239,68,68,0.3)',
      }}>
      {msg}
    </div>
  )
}

export function StatusMessages({ error, success }) {
  return (
    <>
      {error && <ErrorBanner msg={error} />}
      {success && (
        <div className="text-sm p-3 rounded mb-4"
          style={{
            background: 'rgba(34,197,94,0.1)',
            color: '#22c55e',
            border: '1px solid rgba(34,197,94,0.3)',
          }}>
          {success}
        </div>
      )}
    </>
  )
}
