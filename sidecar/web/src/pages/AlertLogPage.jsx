import { Bell } from 'lucide-react'
import { useAPI } from '../hooks/useAPI'
import { DataTable } from '../components/DataTable'
import { TimeAgo } from '../components/TimeAgo'
import { SeverityBadge } from '../components/SeverityBadge'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'

const STATUS_COLORS = {
  sent: 'var(--green)',
  failed: 'var(--red)',
  skipped: '#fbbf24',
}

const CHANNEL_STYLES = {
  slack: { bg: '#1a2332', text: '#4a9eff' },
  pagerduty: { bg: '#2d1a1a', text: '#ef4444' },
  webhook: { bg: '#1a2e1a', text: '#22c55e' },
}

function Summary({ alerts }) {
  const byChannel = {}
  const bySeverity = {}
  for (const a of alerts) {
    byChannel[a.channel] = (byChannel[a.channel] || 0) + 1
    bySeverity[a.severity] = (bySeverity[a.severity] || 0) + 1
  }

  return (
    <div className="flex gap-4 mb-6 flex-wrap">
      <div className="rounded-lg px-5 py-3 flex items-center gap-3"
        style={{
          background: 'var(--bg-card)',
          border: '1px solid var(--border)',
        }}>
        <Bell size={18} style={{ color: 'var(--accent)' }} />
        <div>
          <div className="text-2xl font-bold"
            style={{ color: 'var(--text-primary)' }}>
            {alerts.length}
          </div>
          <div className="text-xs"
            style={{ color: 'var(--text-secondary)' }}>
            Total Alerts
          </div>
        </div>
      </div>

      {Object.entries(bySeverity).map(([sev, count]) => (
        <div key={sev} className="rounded-lg px-5 py-3"
          style={{
            background: 'var(--bg-card)',
            border: '1px solid var(--border)',
          }}>
          <div className="text-2xl font-bold"
            style={{ color: 'var(--text-primary)' }}>
            {count}
          </div>
          <div className="text-xs">
            <SeverityBadge severity={sev} />
          </div>
        </div>
      ))}

      {Object.entries(byChannel).map(([ch, count]) => {
        const style = CHANNEL_STYLES[ch] || CHANNEL_STYLES.webhook
        return (
          <div key={ch} className="rounded-lg px-5 py-3"
            style={{
              background: 'var(--bg-card)',
              border: '1px solid var(--border)',
            }}>
            <div className="text-2xl font-bold"
              style={{ color: 'var(--text-primary)' }}>
              {count}
            </div>
            <div className="text-xs">
              <span className="px-2 py-0.5 rounded text-xs font-medium"
                style={{ background: style.bg, color: style.text }}>
                {ch}
              </span>
            </div>
          </div>
        )
      })}
    </div>
  )
}

export function AlertLogPage({ database }) {
  const dbParam = database && database !== 'all'
    ? `?database=${database}` : ''
  const { data, loading, error, refetch } =
    useAPI(`/api/v1/alert-log${dbParam}`)

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  const alerts = data?.alerts || []

  if (alerts.length === 0) {
    return <EmptyState message="No alerts recorded" />
  }

  const columns = [
    {
      key: 'sent_at', label: 'Sent At',
      render: r => <TimeAgo timestamp={r.sent_at} />,
    },
    {
      key: 'severity', label: 'Severity',
      render: r => <SeverityBadge severity={r.severity} />,
    },
    { key: 'title', label: 'Title' },
    { key: 'category', label: 'Category' },
    {
      key: 'channel', label: 'Channel',
      render: r => {
        const style =
          CHANNEL_STYLES[r.channel] || CHANNEL_STYLES.webhook
        return (
          <span className="px-2 py-0.5 rounded text-xs font-medium"
            style={{
              background: style.bg, color: style.text,
            }}>
            {r.channel}
          </span>
        )
      },
    },
    {
      key: 'status', label: 'Status',
      render: r => (
        <span style={{
          color: STATUS_COLORS[r.status]
            || 'var(--text-secondary)',
        }}>
          {r.status}
        </span>
      ),
    },
  ]

  return (
    <>
      <Summary alerts={alerts} />
      <DataTable columns={columns} rows={alerts} />
    </>
  )
}
