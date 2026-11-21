import { Layers, ShieldCheck } from 'lucide-react'
import { TrustBadge } from './TrustBadge'
import { useAPI } from '../hooks/useAPI'
import {
  fleetAutonomy, fleetAutonomyHint, fleetBadge, modeBadgeLabel, modeCounts,
  rowAutoFamilies, stoppedRows,
} from '../lib/autonomy'

const MIXED_STATIC_HINT = 'Autonomy differs by database. Select a database'
  + ' to see what it may run without approval'

const pill = {
  background: 'var(--bg-hover)',
  color: 'var(--text-primary)',
  border: '1px solid var(--border)',
  lineHeight: 1.4,
}

// HeaderTrustBadge says what pg_sage may do right now. With the Autonomy
// page's modes (ux-06b) it counts classes per mode ("12 ask first · 3 act
// in window") for the selected database or the fleet, and an emergency
// stop is named first. Without modes (no ledger, an older server) it
// falls back to the level with its live families (dashboard-02).
export function HeaderTrustBadge({ rows, selectedDB }) {
  const one = selectedDB && selectedDB !== 'all'
  const trust = useAPI(one ? `/api/v1/trust?database=${encodeURIComponent(selectedDB)}`
    : '/api/v1/trust', 60000)
  if (!Array.isArray(rows) || rows.length === 0) return null
  const label = modeBadgeLabel(modeCounts(trust?.data))
  if (!label) return <LevelBadge rows={rows} selectedDB={selectedDB} />
  // The fleet row's stop flag is fresher than the minute-old trust read.
  const stopped = stoppedRows(one ? rows.filter(r => r.name === selectedDB) : rows)
  if (stopped.length > 0) {
    return <ModePill testId="autonomy-badge-stopped"
      label={`Emergency stop on ${stopped.length} · ${label}`}
      hint={`Emergency stop on ${stopped.join(', ')}: nothing runs there`} />
  }
  return <ModePill testId="autonomy-badge" label={label}
    hint="Classes per autonomy mode. Open Autonomy for each class and why" />
}

function ModePill({ testId, label, hint }) {
  return (
    <a href="#/autonomy" data-testid={testId} title={hint}
      className="inline-flex items-center gap-1 px-2 py-0.5 rounded text-xs font-semibold"
      style={pill}>
      <ShieldCheck size={12} aria-hidden="true" />
      {label}
    </a>
  )
}

function LevelBadge({ rows, selectedDB }) {
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
      style={pill}>
      <Layers size={12} aria-hidden="true" />
      {badge.label}
    </span>
  )
}
