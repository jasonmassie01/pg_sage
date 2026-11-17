import { useEffect, useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { ErrorBanner } from '../../components/ErrorBanner'
import { LiveTimeAgo } from '../../components/TimeAgo'
import { PlanView } from './PlanView'
import { PLAN_STATUS, planDatabaseParam, planHref, planParam } from './plans'

// PlansTab is the Plans tab of Actions (ux-03): multi-step changes, newest
// first, from plan-03a's GET /api/v1/plans; ?plan=<id> (with the plan's
// database=) opens one plan's review. Cases link here ("Part of plan")
// and so does the Inbox's plan_approval item.

const muted = { color: 'var(--text-secondary)' }

function usePlanFocus() {
  const read = () => ({ id: planParam(), database: planDatabaseParam() })
  const [focus, setFocus] = useState(read)
  useEffect(() => {
    const changed = () => setFocus(read())
    window.addEventListener('hashchange', changed)
    return () => window.removeEventListener('hashchange', changed)
  }, [])
  return focus
}

function listURL(database) {
  return database && database !== 'all'
    ? `/api/v1/plans?database=${encodeURIComponent(database)}` : '/api/v1/plans'
}

function PlanList({ database }) {
  const res = useAPI(listURL(database), 30000)
  if (res.error && !res.data) {
    return <ErrorBanner message={`Plans could not be loaded: ${res.error}`}
      onRetry={res.refetch} />
  }
  if (!res.data) return <p className="text-sm" style={muted}>Loading plans…</p>
  const plans = Array.isArray(res.data.plans) ? res.data.plans : []
  const failed = Array.isArray(res.data.errors) ? res.data.errors : []
  return <div className="space-y-2">
    {failed.length > 0 && <p data-testid="plans-errors" className="text-sm"
      style={{ color: 'var(--yellow)' }}>Plans could not be read from{' '}
      {failed.map(f => f.database).join(', ')}; their plans are missing, not empty.</p>}
    {plans.length === 0 ? <p className="text-sm" style={muted}>No plans yet.</p>
      : <ul data-testid="plans-list" className="space-y-2">
        {plans.map(p => <li key={`${p.database}-${p.id}`} className="rounded border p-3
          text-sm" style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
          <a href={planHref(p.id, p.database)} className="font-medium underline"
            style={{ color: 'var(--accent)' }}>{p.goal || `Plan ${p.id}`}</a>
          <div className="text-xs mt-1" style={muted}>
            {PLAN_STATUS[p.status] || p.status} · {p.database} · made{' '}
            <LiveTimeAgo timestamp={p.created_at} /></div>
        </li>)}
      </ul>}
  </div>
}

export function PlansTab({ database, canReview }) {
  const focus = usePlanFocus()
  if (!focus.id) return <PlanList database={database} />
  return <div className="space-y-2">
    <a href="#/actions?tab=plans" className="text-sm underline"
      style={{ color: 'var(--accent)' }}>All plans</a>
    <PlanView key={`${focus.database}-${focus.id}`} id={focus.id}
      database={focus.database || database} canReview={canReview} />
  </div>
}
