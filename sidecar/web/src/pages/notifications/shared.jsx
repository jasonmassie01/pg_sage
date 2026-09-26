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

// Mirrors notify.EventSeverity: the severity each event is emitted with.
// A rule whose min_severity is above it can never fire, so new rules
// default to the event's own severity (notify.DefaultMinSeverity).
// Incident events carry variable severity; warning is the default.
export const EVENT_SEVERITY = {
  action_executed: 'info',
  action_failed: 'warning',
  approval_needed: 'warning',
  finding_critical: 'critical',
  query_rewrite_suggested: 'warning',
  incident_detected: 'warning',
  incident_escalated: 'warning',
  incident_resolved: 'warning',
}

export const EVENT_TYPES = Object.keys(EVENT_SEVERITY)

export function defaultMinSeverity(event) {
  return EVENT_SEVERITY[event] || 'info'
}

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
