import { History } from 'lucide-react'

function formatDate(value) {
  if (!value) return 'n/a'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return 'n/a'
  return date.toLocaleString()
}

function detailSummary(detail) {
  if (!detail || typeof detail !== 'object') return ''
  const pairs = Object.entries(detail)
    .filter(([, value]) => value !== null && value !== undefined && value !== '')
    .slice(0, 3)
  return pairs.map(([key, value]) => `${key}: ${String(value)}`).join(' / ')
}

export function AuditEventsPanel({ events }) {
  const visible = events.slice(0, 8)
  return (
    <div data-testid="agent-db-audit">
      <div className="mb-2 flex items-center gap-2">
        <History size={15} style={{ color: 'var(--text-secondary)' }} />
        <h3 className="text-xs font-semibold"
          style={{ color: 'var(--text-secondary)' }}>
          Audit events
        </h3>
      </div>
      {visible.length === 0 ? (
        <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          None
        </p>
      ) : (
        <div className="space-y-2">
          {visible.map(event => (
            <div key={event.audit_id || `${event.event}-${event.created_at}`}
              className="rounded border p-2"
              style={{ borderColor: 'var(--border)' }}>
              <div className="flex items-center justify-between gap-2">
                <span className="text-sm"
                  style={{ color: 'var(--text-primary)' }}>
                  {event.event}
                </span>
                <span className="text-xs"
                  style={{ color: 'var(--text-secondary)' }}>
                  {formatDate(event.created_at)}
                </span>
              </div>
              {detailSummary(event.detail) && (
                <div className="mt-1 text-xs"
                  style={{ color: 'var(--text-secondary)' }}>
                  {detailSummary(event.detail)}
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
