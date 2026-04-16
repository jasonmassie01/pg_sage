import { useState } from 'react'
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
  skipped: 'var(--yellow)',
}

const CHANNEL_STYLES = {
  slack: {
    bg: 'var(--channel-slack-bg, #1a2332)',
    text: 'var(--channel-slack-text, #4a9eff)',
  },
  pagerduty: {
    bg: 'var(--channel-pagerduty-bg, #2d1a1a)',
    text: 'var(--channel-pagerduty-text, var(--red))',
  },
  webhook: {
    bg: 'var(--channel-webhook-bg, #1a2e1a)',
    text: 'var(--channel-webhook-text, var(--green))',
  },
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

function filterByDate(alerts, from, to) {
  if (!from && !to) return alerts
  const fromTs = from ? new Date(from + 'T00:00:00').getTime() : null
  const toTs = to ? new Date(to + 'T23:59:59').getTime() : null
  return alerts.filter(a => {
    if (!a.sent_at) return false
    const t = new Date(a.sent_at).getTime()
    if (fromTs !== null && t < fromTs) return false
    if (toTs !== null && t > toTs) return false
    return true
  })
}

function DateRangeFilter({ from, to, onFrom, onTo, onClear }) {
  const inputStyle = {
    background: 'var(--bg-card)',
    color: 'var(--text-primary)',
    border: '1px solid var(--border)',
  }
  return (
    <div className="flex items-center gap-2 mb-4 flex-wrap">
      <label className="text-xs"
        style={{ color: 'var(--text-secondary)' }}>
        From
      </label>
      <input type="date" value={from}
        data-testid="alert-log-date-from"
        onChange={e => onFrom(e.target.value)}
        className="px-2 py-1 rounded text-sm"
        style={inputStyle} />
      <label className="text-xs"
        style={{ color: 'var(--text-secondary)' }}>
        To
      </label>
      <input type="date" value={to}
        data-testid="alert-log-date-to"
        onChange={e => onTo(e.target.value)}
        className="px-2 py-1 rounded text-sm"
        style={inputStyle} />
      {(from || to) && (
        <button onClick={onClear}
          className="px-2 py-1 rounded text-xs"
          style={{
            color: 'var(--text-secondary)',
            border: '1px solid var(--border)',
          }}>
          Clear
        </button>
      )}
    </div>
  )
}

export function AlertLogPage({ database }) {
  const dbParam = database && database !== 'all'
    ? `?database=${database}` : ''
  const { data, loading, error, refetch } =
    useAPI(`/api/v1/alert-log${dbParam}`)
  const [fromDate, setFromDate] = useState('')
  const [toDate, setToDate] = useState('')

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  const allAlerts = data?.alerts || []
  const alerts = filterByDate(allAlerts, fromDate, toDate)

  if (allAlerts.length === 0) {
    return <EmptyState message="No alerts sent yet. Configure notification channels in Settings to receive alerts when pg_sage detects issues." />
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
      <DateRangeFilter from={fromDate} to={toDate}
        onFrom={setFromDate} onTo={setToDate}
        onClear={() => { setFromDate(''); setToDate('') }} />
      <Summary alerts={alerts} />
      {alerts.length === 0 ? (
        <EmptyState message="No alerts match the selected date range." />
      ) : (
        <DataTable columns={columns} rows={alerts} />
      )}
    </>
  )
}
