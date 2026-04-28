import { Server, AlertTriangle, CheckCircle } from 'lucide-react'
import { StatusDot } from './StatusDot'
import { TrustBadge } from './TrustBadge'
import { formatTrustLevel } from '../pages/Dashboard'

function healthColor(score) {
  if (score === undefined || score === null) return 'var(--text-secondary)'
  if (score >= 85) return 'var(--green)'
  if (score >= 60) return 'var(--yellow)'
  return 'var(--red)'
}

export function DatabaseTile({ db, selected, onSelect }) {
  const s = db.status || {}
  const critical = s.findings_critical || 0
  const warning = s.findings_warning || 0
  const info = s.findings_info || 0
  const healthy = critical === 0 && warning === 0
  const trust = s.trust_level || db.trust_level || null
  const caps = s.capabilities || {}
  const provider = s.platform || caps.provider || 'unknown'
  const blocker = (caps.blockers || [])[0]
  const ready = caps.ready_for_auto_safe

  return (
    <button
      type="button"
      data-testid="db-list-item"
      onClick={() => onSelect?.(db.name)}
      className="rounded p-4 text-left transition-colors"
      style={{
        background: selected ? 'var(--bg-hover)' : 'var(--bg-card)',
        border: `1px solid ${
          selected ? 'var(--accent)' : 'var(--border)'}`,
        cursor: 'pointer',
      }}>
      <div className="flex items-center gap-2 mb-2">
        <StatusDot connected={s.connected} error={s.error} />
        <Server size={14}
          style={{ color: 'var(--text-secondary)' }} />
        <span className="font-medium flex-1 truncate"
          style={{ color: 'var(--text-primary)' }}>
          {db.name}
        </span>
      </div>
      <div className="flex items-center gap-3 mb-2">
        <div>
          <div className="text-[10px] uppercase tracking-wider"
            style={{ color: 'var(--text-secondary)' }}>
            Health
          </div>
          <div className="text-2xl font-bold"
            style={{ color: healthColor(s.health_score) }}>
            {s.health_score ?? '—'}
          </div>
        </div>
        <div className="flex-1 flex flex-wrap gap-1">
          {healthy ? (
            <span className="flex items-center gap-1 text-xs px-1.5 py-0.5 rounded"
              style={{
                background: 'rgba(34,197,94,0.12)',
                color: 'var(--green)',
              }}>
              <CheckCircle size={11} /> Clean
            </span>
          ) : (
            <>
              {critical > 0 && (
                <span className="flex items-center gap-1 text-xs px-1.5 py-0.5 rounded"
                  style={{
                    background: 'rgba(239,68,68,0.15)',
                    color: 'var(--red)',
                  }}>
                  <AlertTriangle size={11} /> {critical} critical
                </span>
              )}
              {warning > 0 && (
                <span className="text-xs px-1.5 py-0.5 rounded"
                  style={{
                    background: 'rgba(245,158,11,0.15)',
                    color: 'var(--yellow)',
                  }}>
                  {warning} warning
                </span>
              )}
              {info > 0 && (
                <span className="text-xs px-1.5 py-0.5 rounded"
                  style={{
                    background: 'rgba(59,130,246,0.15)',
                    color: 'var(--blue, #3b82f6)',
                  }}>
                  {info} info
                </span>
              )}
            </>
          )}
        </div>
      </div>
      {trust && (
        <div className="flex items-center justify-between">
          <span className="text-[10px] uppercase tracking-wider"
            style={{ color: 'var(--text-secondary)' }}>
            {formatTrustLevel(trust) || trust}
          </span>
          <TrustBadge level={trust} compact />
        </div>
      )}
      <div className="mt-3 pt-2"
        style={{ borderTop: '1px solid var(--border)' }}>
        <div className="flex items-center justify-between gap-2">
          <span className="text-xs truncate"
            style={{ color: 'var(--text-secondary)' }}>
            {provider}
          </span>
          <span className="text-xs"
            style={{ color: ready ? 'var(--green)' : 'var(--yellow)' }}>
            {ready ? 'Auto-safe ready' : 'Auto-safe blocked'}
          </span>
        </div>
        {blocker && (
          <div className="text-xs mt-1 truncate"
            style={{ color: 'var(--text-secondary)' }}>
            {blocker}
          </div>
        )}
      </div>
    </button>
  )
}
