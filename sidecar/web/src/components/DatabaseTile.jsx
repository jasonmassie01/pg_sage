import { Server, AlertTriangle, CheckCircle } from 'lucide-react'
import { StatusDot } from './StatusDot'
import { TrustBadge } from './TrustBadge'
import { formatTrustLevel } from '../pages/Dashboard'
import { rowAutoFamilies } from '../lib/autonomy'
import { instanceOwnerLabel } from '../lib/instanceOwner'

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
  // A disconnected or erroring database is never "Clean" (G9-B30).
  const healthy = critical === 0 && warning === 0
    && s.connected === true && !s.error
  const trust = s.trust_level || db.trust_level || null
  const caps = s.capabilities || {}
  const provider = s.platform || caps.provider || 'unknown'
  const blocker = (caps.blockers || [])[0]
  const ready = caps.ready_for_auto_safe
  const readiness = ready === true ? 'ready' : ready === false ? 'blocked' : 'unknown'
  const instanceOwner = instanceOwnerLabel(caps)
  const reasons = Array.isArray(s.health_reasons) ? s.health_reasons : []
  const haRole = typeof s.ha_role === 'string' && s.ha_role ? s.ha_role : null

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
            title={reasons.length > 0 ? reasons.join('; ') : undefined}
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
      {reasons.length > 0 && (
        <ul data-testid="health-reasons" className="text-xs mb-2"
          style={{ color: 'var(--text-secondary)' }}>
          {reasons.map(reason => (
            <li key={reason} className="truncate">{reason}</li>
          ))}
        </ul>
      )}
      {trust && (
        <div className="flex items-center justify-between">
          <span className="text-[10px] uppercase tracking-wider"
            style={{ color: 'var(--text-secondary)' }}>
            {formatTrustLevel(trust) || trust}
          </span>
          <TrustBadge level={trust} compact
            autoFamilies={rowAutoFamilies(db)} />
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
            style={{ color: ready === true ? 'var(--green)'
              : ready === false ? 'var(--yellow)' : 'var(--text-secondary)' }}>
            Auto-safe {readiness}
          </span>
        </div>
        {instanceOwner && (
          <div data-testid="db-instance-owner" className="text-xs mt-1 truncate"
            title={instanceOwner.title}
            style={{ color: 'var(--text-secondary)' }}>
            {instanceOwner.text}
          </div>
        )}
        {blocker && (
          <div className="text-xs mt-1 truncate"
            style={{ color: 'var(--text-secondary)' }}>
            {blocker}
          </div>
        )}
        {haRole && (
          <HAStatus role={haRole} safeMode={s.ha_safe_mode === true}
            lastChange={s.ha_last_change} />
        )}
      </div>
    </button>
  )
}

// HAStatus shows the database's HA role and failover safe mode (fleet-04).
// Only a confirmed primary outside safe mode allows autonomous actions.
function HAStatus({ role, safeMode, lastChange }) {
  const writable = role === 'primary' && !safeMode
  return (
    <div className="flex items-center justify-between gap-2 mt-1">
      <span data-testid="ha-role" className="text-xs"
        style={{ color: writable ? 'var(--text-secondary)' : 'var(--yellow)' }}>
        HA {role}
      </span>
      {safeMode && (
        <span data-testid="ha-safe-mode"
          className="text-xs px-1.5 py-0.5 rounded"
          title={lastChange ? `Last role change ${lastChange}` : undefined}
          style={{
            background: 'rgba(245,158,11,0.15)',
            color: 'var(--yellow)',
          }}>
          HA safe mode
        </span>
      )}
    </div>
  )
}

