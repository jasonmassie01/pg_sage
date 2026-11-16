import { useMemo, useRef, useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { ErrorBanner } from '../../components/ErrorBanner'
import { PlanApproval, VOIDED } from './PlanApproval'
import { PlanStageRow } from './PlanStageRow'
import {
  PLAN_STATUS, expectedText, haltSummary, originText, outcomesSince, postPlanDecision,
  sortedSteps, stagesText,
} from './plans'

// PlanView is one plan at #/actions?tab=plans&plan=<id>&database=<db>
// (ux-03): the header (goal, scope, expected effect, risk, origin,
// evidence pack), a halted plan's summary, the approval controls and the
// ordered stages, polled from plan-03a's GET /api/v1/plans/{id}. Measured
// effects come from ux-02a's outcome ledger, read only once a stage ran.

const muted = { color: 'var(--text-secondary)' }
const STEP_APPROVABLE = new Set(['pending', 'awaiting_approval'])

function dbQuery(db) {
  return db && db !== 'all' ? `?database=${encodeURIComponent(db)}` : ''
}

function scopeText(scope) {
  const parts = []
  if (scope?.objects?.length) parts.push(scope.objects.join(', '))
  const q = scope?.queryids || []
  if (q.length) parts.push(`${q.length === 1 ? 'query' : 'queries'} ${q.join(', ')}`)
  return parts.join(' · ') || 'not stated'
}

function riskText(coverage) {
  const never = (coverage || []).filter(c => c.needs_own_approval).map(c => c.ordinal)
  if (never.length === 0) return 'every stage can run under one approval'
  return `${stagesText(never)} always ${never.length === 1 ? 'needs its' : 'need their'} `
    + 'own approval'
}

function PlanHeader({ detail, steps }) {
  const p = detail.plan
  const expected = steps.map(s => `stage ${s.ordinal} ${expectedText(s)}`).join('; ')
  return <div data-testid="plan-header" className="rounded border p-3 space-y-1 text-sm"
    style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
    <div className="flex flex-wrap items-center justify-between gap-2">
      <h3 className="font-medium" style={{ color: 'var(--text-primary)' }}>{p.goal}</h3>
      <span className="text-xs" style={muted}>{PLAN_STATUS[p.status] || p.status}</span>
    </div>
    <p style={muted}>Scope: {scopeText(p.scope)}</p>
    <p style={muted}>Expected: {expected || 'no stages'}</p>
    <p style={muted}>Risk: {riskText(detail.coverage)}</p>
    <p style={muted}>Origin: {originText(p)}</p>
    {p.pack_hash && <p className="font-mono text-xs break-all" style={muted}>
      Evidence pack {p.pack_hash}</p>}
    {p.pause_reason && <p style={{ color: 'var(--yellow)' }}>Paused: {p.pause_reason}</p>}
  </div>
}

// voided: the plan has approvals but none live at its current hash.
function voided(detail) {
  const list = Array.isArray(detail.approvals) ? detail.approvals : []
  const hash = detail.plan_hash || detail.plan.plan_hash
  return list.length > 0 && !list.some(a => !a.voided_at && a.plan_hash === hash)
}

function HaltSummary({ plan }) {
  const h = haltSummary(plan)
  if (!h) return null
  const line = (label, list) => list.length > 0 && <p>{label}: {stagesText(list)}</p>
  return <div data-testid="plan-halted" role="status" className="rounded border p-3 text-sm
    space-y-1" style={{ borderColor: 'var(--yellow)', color: 'var(--text-primary)' }}>
    <p className="font-medium">{h.halted ? `Halted at stage ${h.halted}` : 'Halted'}
      {h.reason ? `: ${h.reason}` : ''}</p>
    {line('Reverted', h.reverted)}
    {line('Kept', h.kept)}
    {line('Never started', h.notStarted)}
  </div>
}

function useOutcomes(detail, pollMs) {
  const ran = sortedSteps(detail?.plan).some(s => s.runtime?.action_log_id)
  const db = detail?.database
  const created = detail?.plan?.created_at
  const url = useMemo(() => (ran && db ? `/api/v1/outcomes?${new URLSearchParams({
    database: db, since: outcomesSince(created), limit: '500' })}` : null), [ran, db, created])
  const res = useAPI(url, pollMs)
  const rows = Array.isArray(res.data?.rows) ? res.data.rows : []
  return new Map(rows.map(r => [r.action_id, r]))
}

function useStepApproval(detail, database, refetch) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const inFlight = useRef(false)
  const approve = async step => {
    if (inFlight.current) return
    inFlight.current = true
    setBusy(true)
    setError('')
    const url = `/api/v1/plans/${encodeURIComponent(detail.plan.id)}/steps/`
      + `${encodeURIComponent(step.id)}/approve${dbQuery(detail.database || database)}`
    try {
      const res = await postPlanDecision(url, { plan_hash: detail.plan_hash })
      if (!res.ok) setError(res.code === 'content_changed' ? VOIDED : res.message)
      if (res.ok || res.code === 'content_changed') refetch()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      inFlight.current = false
      setBusy(false)
    }
  }
  return { busy, error, approve }
}

function stepApprovable(detail, step, cov, canReview) {
  const live = !voided(detail) && (detail.approvals || []).length > 0
  return canReview && live && ['approved', 'running'].includes(detail.plan.status)
    && step.kind === 'action' && cov && !cov.covered
    && STEP_APPROVABLE.has(step.runtime?.state || 'pending')
}

function Stages({ detail, steps, outcomes, canReview, stepApproval }) {
  const cov = new Map((detail.coverage || []).map(c => [c.ordinal, c]))
  if (steps.length === 0) {
    return detail.plan.status === 'proposed' ? null
      : <p className="text-sm" style={muted}>This plan has no steps</p>
  }
  return <ul data-testid="plan-stages" className="space-y-2"
    style={{ maxHeight: '70vh', overflowY: 'auto' }}>
    {steps.map((s, i) => <PlanStageRow key={s.ordinal} step={s} edges={detail.plan.edges}
      next={i + 1 < steps.length ? cov.get(steps[i + 1].ordinal) || null : null}
      outcome={outcomes.get(s.runtime?.action_log_id)} busy={stepApproval.busy}
      onApproveStep={stepApprovable(detail, s, cov.get(s.ordinal), canReview)
        ? stepApproval.approve : null} />)}
  </ul>
}

export function PlanView({ id, database, canReview = false, pollMs = 5000 }) {
  const plan = useAPI(`/api/v1/plans/${encodeURIComponent(id)}${dbQuery(database)}`, pollMs)
  const detail = plan.data?.plan ? plan.data : null
  const outcomes = useOutcomes(detail, pollMs)
  const stepApproval = useStepApproval(detail, database, plan.refetch)
  if (!detail) {
    return plan.error ? <ErrorBanner message={`The plan could not be loaded: ${plan.error}`}
      onRetry={plan.refetch} />
      : <p className="text-sm" style={muted}>Loading the plan…</p>
  }
  const steps = sortedSteps(detail.plan)
  return <div className="space-y-3" data-testid="plan-view">
    {plan.error && <ErrorBanner message={`The plan could not be refreshed: ${plan.error}`}
      onRetry={plan.refetch} />}
    <PlanHeader detail={detail} steps={steps} />
    {voided(detail) && <p data-testid="plan-voided" className="text-sm"
      style={{ color: 'var(--yellow)' }}>{VOIDED}</p>}
    <HaltSummary plan={detail.plan} />
    <PlanApproval detail={detail} database={database} canReview={canReview}
      onDecided={plan.refetch} onChanged={plan.refetch} />
    {stepApproval.error && <p role="alert" className="text-sm"
      style={{ color: 'var(--red)' }}>{stepApproval.error}</p>}
    <Stages detail={detail} steps={steps} outcomes={outcomes} canReview={canReview}
      stepApproval={stepApproval} />
  </div>
}
