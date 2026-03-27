import { useAPI } from '../hooks/useAPI'
import { SeverityBadge } from '../components/SeverityBadge'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { EmptyState } from '../components/EmptyState'
import { TimeAgo } from '../components/TimeAgo'
import { TrendingUp } from 'lucide-react'

function formatCategory(category) {
  return (category || '')
    .replace(/^forecast_/, '')
    .replace(/_/g, ' ')
    .replace(/\b\w/g, c => c.toUpperCase())
}

function formatDetailKey(key) {
  return key
    .replace(/_/g, ' ')
    .replace(/\b\w/g, c => c.toUpperCase())
}

function SummaryCard({ label, count, color }) {
  return (
    <div className="flex flex-col items-center p-4 rounded"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <span className="text-2xl font-bold" style={{ color }}>
        {count}
      </span>
      <span className="text-xs mt-1"
        style={{ color: 'var(--text-secondary)' }}>
        {label}
      </span>
    </div>
  )
}

function ForecastCard({ forecast }) {
  const detail = forecast.detail || {}
  const detailEntries = Object.entries(detail)

  return (
    <div className="p-4 rounded space-y-3"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2">
          <SeverityBadge severity={forecast.severity} />
          <span className="text-sm font-medium"
            style={{ color: 'var(--text-primary)' }}>
            {forecast.title}
          </span>
        </div>
        <div className="flex items-center gap-3 text-xs">
          <span style={{ color: 'var(--text-secondary)' }}>
            {forecast.occurrence_count}x
          </span>
          <TimeAgo timestamp={forecast.last_seen} />
        </div>
      </div>

      <div className="flex items-center gap-2">
        <TrendingUp size={14} style={{ color: 'var(--text-secondary)' }} />
        <span className="text-xs"
          style={{ color: 'var(--text-secondary)' }}>
          {formatCategory(forecast.category)}
        </span>
        {forecast.object_identifier && (
          <span className="text-xs px-1.5 py-0.5 rounded"
            style={{
              background: 'var(--bg-hover)',
              color: 'var(--text-secondary)',
            }}>
            {forecast.object_identifier}
          </span>
        )}
      </div>

      {detailEntries.length > 0 && (
        <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
          {detailEntries.map(([key, value]) => (
            <div key={key} className="text-xs p-2 rounded"
              style={{ background: 'var(--bg-primary)' }}>
              <div style={{ color: 'var(--text-secondary)' }}>
                {formatDetailKey(key)}
              </div>
              <div className="font-medium mt-0.5"
                style={{ color: 'var(--text-primary)' }}>
                {typeof value === 'number'
                  ? Number.isInteger(value) ? value : value.toFixed(2)
                  : String(value)}
              </div>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

export function ForecastsPage({ database }) {
  const dbParam = database && database !== 'all'
    ? `?database=${database}` : ''
  const url = `/api/v1/forecasts${dbParam}`
  const { data, loading, error, refetch } = useAPI(url)

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} onRetry={refetch} />

  const forecasts = data?.forecasts || []

  if (forecasts.length === 0) {
    return <EmptyState message="No workload forecasts available" />
  }

  const counts = { critical: 0, warning: 0, info: 0 }
  for (const f of forecasts) {
    if (counts[f.severity] !== undefined) {
      counts[f.severity]++
    }
  }

  return (
    <div className="space-y-6">
      <div className="grid grid-cols-3 gap-4">
        <SummaryCard label="Critical" count={counts.critical}
          color="#ef4444" />
        <SummaryCard label="Warning" count={counts.warning}
          color="#fbbf24" />
        <SummaryCard label="Info" count={counts.info}
          color="#4a9eff" />
      </div>

      <div className="space-y-3">
        {forecasts.map((f, i) => (
          <ForecastCard key={`${f.category}-${f.object_identifier}-${i}`}
            forecast={f} />
        ))}
      </div>

      <div className="text-xs"
        style={{ color: 'var(--text-secondary)' }}>
        {forecasts.length} forecast{forecasts.length !== 1 ? 's' : ''}
      </div>
    </div>
  )
}
