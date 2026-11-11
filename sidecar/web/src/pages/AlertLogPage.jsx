import { useState } from 'react'
import { Bell } from 'lucide-react'
import { useAPI } from '../hooks/useAPI'
import { DataTable } from '../components/DataTable'
import { TimeAgo } from '../components/TimeAgo'
import { SeverityBadge } from '../components/SeverityBadge'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { QueryState } from '../components/QueryState'
import { queryBlocked } from '../lib/queryState'
import { EmptyState } from '../components/EmptyState'
import { ErrorBanner } from '../components/ErrorBanner'

// STATUS_STYLES covers every status the alerting writer records
// (alerting-09). An unknown status shows its raw text in the secondary
// colour rather than borrowing a meaning it may not have.
const STATUS_STYLES = {
  sent: { color: 'var(--green)', label: 'sent', kind: 'delivered' },
  resolved: { color: 'var(--green)', label: 'resolved', kind: 'delivered' },
  error: { color: 'var(--red)', label: 'error', kind: 'failed' },
  resolve_error: { color: 'var(--red)', label: 'resolve failed', kind: 'failed' },
  deferred: {
    color: 'var(--yellow)', label: 'deferred: quiet hours', kind: 'deferred',
  },
}

function statusStyle(status) {
  return STATUS_STYLES[status]
    || { color: 'var(--text-secondary)', label: status, kind: 'other' }
}

const ERROR_PREVIEW_CHARS = 120

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

const tileStyle = {
  background: 'var(--bg-card)',
  border: '1px solid var(--border)',
}

function Tile({ testId, count, children }) {
  return (
    <div className="rounded-lg px-5 py-3 flex items-center gap-3"
      data-testid={testId} style={tileStyle}>
      <div>
        <div className="text-2xl font-bold"
          style={{ color: 'var(--text-primary)' }}>
          {count}
        </div>
        <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          {children}
        </div>
      </div>
    </div>
  )
}

function OutcomeLabel({ color, children }) {
  return (
    <span className="flex items-center gap-1">
      <Bell size={12} style={{ color }} /> {children}
    </span>
  )
}

function ChannelBadge({ channel }) {
  const style = CHANNEL_STYLES[channel] || CHANNEL_STYLES.webhook
  return (
    <span className="px-2 py-0.5 rounded text-xs font-medium"
      style={{ background: style.bg, color: style.text }}>
      {channel}
    </span>
  )
}

const OUTCOMES = [
  ['delivered', 'Delivered', 'var(--green)'],
  ['failed', 'Failed', 'var(--red)'],
  ['deferred', 'Deferred', 'var(--yellow)'],
]

// Summary counts delivery outcomes. Deferred rows are not deliveries, so
// they stay out of the severity and channel tiles.
function Summary({ alerts }) {
  const byKind = Object.fromEntries(OUTCOMES.map(([kind]) => [kind, 0]))
  const byChannel = {}
  const bySeverity = {}
  for (const a of alerts) {
    const kind = statusStyle(a.status).kind
    if (kind in byKind) byKind[kind] += 1
    if (kind === 'deferred') continue
    byChannel[a.channel] = (byChannel[a.channel] || 0) + 1
    bySeverity[a.severity] = (bySeverity[a.severity] || 0) + 1
  }
  return (
    <div className="flex gap-4 mb-6 flex-wrap" data-testid="alert-log-summary">
      {OUTCOMES.map(([kind, label, color]) => (
        <Tile key={kind} testId={`alert-count-${kind}`} count={byKind[kind]}>
          <OutcomeLabel color={color}>{label}</OutcomeLabel>
        </Tile>
      ))}
      {Object.entries(bySeverity).map(([sev, count]) => (
        <Tile key={sev} count={count}><SeverityBadge severity={sev} /></Tile>
      ))}
      {Object.entries(byChannel).map(([ch, count]) => (
        <Tile key={ch} count={count}><ChannelBadge channel={ch} /></Tile>
      ))}
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
  const labelStyle = { color: 'var(--text-secondary)' }
  return (
    <div className="flex items-center gap-2 mb-4 flex-wrap">
      <label className="text-xs" style={labelStyle}>From</label>
      <input type="date" value={from}
        data-testid="alert-log-date-from"
        onChange={e => onFrom(e.target.value)}
        className="px-2 py-1 rounded text-sm"
        style={inputStyle} />
      <label className="text-xs" style={labelStyle}>To</label>
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
      <span className="text-xs" data-testid="alert-log-date-note" style={labelStyle}>
        The date range filters the newest 100 deliveries per database.
      </span>
    </div>
  )
}

function StatusCell({ row }) {
  const style = statusStyle(row.status)
  return (
    <span data-testid={`alert-status-${row.id}`} style={{ color: style.color }}>
      {style.label}
    </span>
  )
}

function ErrorCell({ row }) {
  const [open, setOpen] = useState(false)
  const message = row.error_message || ''
  const long = message.length > ERROR_PREVIEW_CHARS
  const shown = long && !open
    ? message.slice(0, ERROR_PREVIEW_CHARS) + '…' : message
  return (
    <span data-testid={`alert-error-${row.id}`} className="text-xs"
      style={{ color: 'var(--text-secondary)', wordBreak: 'break-word' }}>
      {shown || '-'}
      {long && (
        <button type="button" onClick={() => setOpen(o => !o)}
          className="ml-1 underline" style={{ color: 'var(--accent)' }}>
          {open ? 'less' : 'more'}
        </button>
      )}
    </span>
  )
}

function FailedDatabases({ names }) {
  if (!names || names.length === 0) return null
  return (
    <div className="mb-4" data-testid="alert-log-failed-databases">
      <ErrorBanner message={'The alert log could not be read from: '
        + names.join(', ') + '. Rows from the other databases are shown.'} />
    </div>
  )
}

function alertColumns(fleetView) {
  const columns = [
    { key: 'sent_at', label: 'Sent At', render: r => <TimeAgo timestamp={r.sent_at} /> },
    {
      key: 'severity', label: 'Severity',
      render: r => <SeverityBadge severity={r.severity} />,
    },
    { key: 'title', label: 'Title' },
    { key: 'category', label: 'Category' },
    { key: 'channel', label: 'Channel', render: r => <ChannelBadge channel={r.channel} /> },
    { key: 'status', label: 'Status', render: r => <StatusCell row={r} /> },
    { key: 'error_message', label: 'Error', render: r => <ErrorCell row={r} /> },
  ]
  if (fleetView) columns.splice(1, 0, { key: 'database_name', label: 'Database' })
  return columns
}

export function AlertLogPage({ database }) {
  const dbParam = database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}` : ''
  const alertsQuery = useAPI(`/api/v1/alert-log${dbParam}`)
  const { data } = alertsQuery
  const [fromDate, setFromDate] = useState('')
  const [toDate, setToDate] = useState('')

  if (queryBlocked(alertsQuery)) {
    return <QueryState query={alertsQuery} skeleton={<LoadingSpinner />} />
  }

  const allAlerts = data?.alerts || []
  const failed = data?.failed_databases
  const alerts = filterByDate(allAlerts, fromDate, toDate)

  if (allAlerts.length === 0) {
    return <QueryState query={alertsQuery}>
      <FailedDatabases names={failed} />
      <EmptyState message="No alerts sent yet. Configure notification channels in Notifications to receive alerts when pg_sage detects issues." />
    </QueryState>
  }

  return (
    <QueryState query={alertsQuery}>
      <FailedDatabases names={failed} />
      <DateRangeFilter from={fromDate} to={toDate}
        onFrom={setFromDate} onTo={setToDate}
        onClear={() => { setFromDate(''); setToDate('') }} />
      <Summary alerts={alerts} />
      {alerts.length === 0 ? (
        <EmptyState message="No alerts match the selected date range." />
      ) : (
        <DataTable columns={alertColumns(database === 'all')} rows={alerts}
          rowKey={r => `${r.database_name || ''}:${r.id}`} />
      )}
    </QueryState>
  )
}
