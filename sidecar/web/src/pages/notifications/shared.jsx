/* eslint-disable react-refresh/only-export-components */
import { ErrorBanner } from '../../components/ErrorBanner'

export const inputStyle = {
  background: 'var(--bg-main)',
  border: '1px solid var(--border)',
  color: 'var(--text-primary)',
}

export const cardStyle = {
  background: 'var(--bg-card)',
  border: '1px solid var(--border)',
}

// alerting-06b: rows reconciled from the config file's alerting: block are
// managed_by "config"; the API refuses to change them (409), so the page
// shows where they come from and offers no edit, toggle or delete.
export const CONFIG_MANAGED_TITLE =
  "Set by the alerting: block of the config file; change it there"

export function isConfigManaged(row) {
  return row?.managed_by === 'config'
}

export function ConfigBadge() {
  return (
    <span data-testid="config-managed-badge" title={CONFIG_MANAGED_TITLE}
      className="ml-2 text-xs px-1.5 py-0.5 rounded whitespace-nowrap"
      style={{ border: '1px solid var(--border)', color: 'var(--text-secondary)' }}>
      From config file
    </span>
  )
}

// Mirrors notify.EventSeverity: the severity each event is emitted with.
// A rule whose min_severity is above it can never fire, so new rules
// default to the event's own severity (notify.DefaultMinSeverity).
// Incident and finding_opened/finding_resolved events carry variable
// severity; warning is the default.
export const EVENT_SEVERITY = {
  action_executed: 'info',
  action_failed: 'warning',
  approval_needed: 'warning',
  finding_critical: 'critical',
  finding_opened: 'warning',
  finding_resolved: 'warning',
  query_rewrite_suggested: 'warning',
  incident_detected: 'warning',
  incident_escalated: 'warning',
  incident_resolved: 'warning',
  custodian_escalation: 'critical',
  fact_expired: 'warning',
  canary_interrupted: 'warning',
}

export const EVENT_TYPES = Object.keys(EVENT_SEVERITY)

export function defaultMinSeverity(event) {
  return EVENT_SEVERITY[event] || 'info'
}

export const SEVERITIES = ['info', 'warning', 'critical']

// Delivery status colours come from the theme tokens (alerting-09); an
// unknown status keeps the secondary colour instead of reading as failed.
export const DELIVERY_STATUS_COLORS = {
  sent: 'var(--green)',
  retrying: 'var(--yellow)',
  error: 'var(--red)',
}

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

export function StatusMessages({ error, success }) {
  return (
    <>
      {error && <div className="mb-4"><ErrorBanner message={error} /></div>}
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
