import { Layers } from 'lucide-react'
import { TrustBadge } from './TrustBadge'
import {
  fleetAutonomy, fleetAutonomyHint, fleetBadge, rowAutoFamilies,
} from '../lib/autonomy'

const MIXED_STATIC_HINT = 'Autonomy differs by database. Select a database'
  + ' to see what it may run without approval'

// HeaderTrustBadge says what pg_sage may do right now: the selected
// database's level with its live families, or for the fleet every level in
// use and, per family, the databases that run it unattended. It never
// collapses a mixed fleet into one level (dashboard-02).
export function HeaderTrustBadge({ rows, selectedDB }) {
  if (!Array.isArray(rows) || rows.length === 0) return null
  if (selectedDB && selectedDB !== 'all') {
    const row = rows.find(r => r.name === selectedDB)
    if (!row?.status?.trust_level) return null
    return <TrustBadge level={row.status.trust_level}
      autoFamilies={rowAutoFamilies(row)} />
  }
  const autonomy = fleetAutonomy(rows)
  const badge = fleetBadge(autonomy)
  if (!badge) return null
  const hint = fleetAutonomyHint(autonomy)
  if (badge.kind === 'single') {
    return <TrustBadge level={badge.level} hint={hint || undefined} />
  }
  return (
    <span data-testid="trust-badge-mixed" title={hint || MIXED_STATIC_HINT}
      className="inline-flex items-center gap-1 px-2 py-0.5 rounded text-xs font-semibold"
      style={{
        background: 'var(--bg-hover)',
        color: 'var(--text-primary)',
        border: '1px solid var(--border)',
        lineHeight: 1.4,
      }}>
      <Layers size={12} aria-hidden="true" />
      {badge.label}
    </span>
  )
}
