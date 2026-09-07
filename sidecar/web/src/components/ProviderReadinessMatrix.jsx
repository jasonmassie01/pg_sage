import { Fragment } from 'react'
import { AlertTriangle, CheckCircle, CircleHelp } from 'lucide-react'
import { useAPI } from '../hooks/useAPI'

function statusText(row, key) {
  const status = row.capabilities?.permissions?.[key]?.status
  return status || 'unknown'
}

function extensionText(row, key) {
  return row.capabilities?.extensions?.[key] || 'unknown'
}

function readinessIcon(row) {
  if (row.ready_for_auto_safe) {
    return <CheckCircle size={14} style={{ color: 'var(--green)' }} />
  }
  if ((row.blockers || []).length > 0) {
    return <AlertTriangle size={14} style={{ color: 'var(--yellow)' }} />
  }
  return <CircleHelp size={14} style={{ color: 'var(--text-secondary)' }} />
}

function blockerText(row) {
  return (row.blockers || [])[0] ||
    (row.capabilities?.limitations || [])[0] ||
    'none'
}

function actionFamilies(row) {
  return row.capabilities?.action_families || []
}

function familyColor(family) {
  if (!family.supported || family.decision === 'blocked') {
    return 'var(--red)'
  }
  if (family.requires_approval || family.decision === 'queue_for_approval') {
    return 'var(--yellow)'
  }
  return 'var(--green)'
}

function familyTitle(family) {
  return [
    family.decision,
    family.blocked_reason,
    family.requires_approval ? 'approval required' : '',
    family.requires_maintenance_window ? 'maintenance window' : '',
  ].filter(Boolean).join(' · ')
}

export function ProviderReadinessMatrix() {
  const { data, loading, error } = useAPI('/api/v1/fleet/readiness')
  const rows = data?.databases || []

  if (loading && rows.length === 0) {
    return (
      <div className="h-24 rounded animate-pulse"
        data-testid="provider-readiness-loading"
        style={{ background: 'var(--bg-hover)' }} />
    )
  }

  if (error || rows.length === 0) return null

  return (
    <div className="rounded p-4"
      data-testid="provider-readiness-matrix"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <div className="flex items-center justify-between gap-3 mb-3">
        <h2 className="text-sm font-medium"
          style={{ color: 'var(--text-secondary)' }}>
          Provider Readiness
        </h2>
        <span className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          {data.summary?.ready_for_auto_safe || 0} auto-safe ready
        </span>
      </div>
      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr style={{ color: 'var(--text-secondary)' }}>
              <th className="text-left font-medium py-2">Database</th>
              <th className="text-left font-medium py-2">Provider</th>
              <th className="text-left font-medium py-2">Auto-safe</th>
              <th className="text-left font-medium py-2">Analyze</th>
              <th className="text-left font-medium py-2">Stats</th>
              <th className="text-left font-medium py-2">Hints</th>
              <th className="text-left font-medium py-2">Replica</th>
              <th className="text-left font-medium py-2">Blocker</th>
            </tr>
          </thead>
          <tbody>
            {rows.map(row => (
              <Fragment key={row.name}>
                <tr key={`${row.name}-summary`}
                  style={{ borderTop: '1px solid var(--border)' }}>
                  <td className="py-2" style={{ color: 'var(--text-primary)' }}>
                    {row.name}
                  </td>
                  <td className="py-2" style={{ color: 'var(--text-secondary)' }}>
                    {row.provider || 'unknown'}
                  </td>
                  <td className="py-2">
                    <span className="inline-flex items-center gap-1"
                      style={{ color: 'var(--text-secondary)' }}>
                      {readinessIcon(row)}
                      {row.ready_for_auto_safe ? 'ready' : 'blocked'}
                    </span>
                  </td>
                  <td className="py-2" style={{ color: 'var(--text-secondary)' }}>
                    {statusText(row, 'analyze')}
                  </td>
                  <td className="py-2" style={{ color: 'var(--text-secondary)' }}>
                    {extensionText(row, 'pg_stat_statements')}
                  </td>
                  <td className="py-2" style={{ color: 'var(--text-secondary)' }}>
                    {extensionText(row, 'pg_hint_plan')}
                  </td>
                  <td className="py-2" style={{ color: 'var(--text-secondary)' }}>
                    {row.capabilities?.is_replica ? 'yes' : 'no'}
                  </td>
                  <td className="py-2" style={{ color: 'var(--text-secondary)' }}>
                    {blockerText(row)}
                  </td>
                </tr>
                {actionFamilies(row).length > 0 && (
                  <tr key={`${row.name}-families`}>
                    <td colSpan={8} className="pb-3">
                      <div className="flex flex-wrap gap-1.5"
                        aria-label={`${row.name} action families`}>
                        {actionFamilies(row).map(family => (
                          <span key={family.action_type}
                            className="inline-flex items-center gap-1 rounded px-2 py-1 text-xs"
                            title={familyTitle(family)}
                            style={{
                              color: familyColor(family),
                              border: '1px solid var(--border)',
                              background: 'var(--bg-muted)',
                            }}>
                            {family.action_type}
                            <span style={{ color: 'var(--text-secondary)' }}>
                              {family.decision}
                            </span>
                          </span>
                        ))}
                      </div>
                    </td>
                  </tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
