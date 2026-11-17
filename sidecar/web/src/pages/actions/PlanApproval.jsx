import { useRef, useState } from 'react'
import { postPlanDecision, sortedSteps, upToChoices } from './plans'

// PlanApproval is a proposed plan's approval controls (ux-03): Approve
// all, Approve up to stage N (only stages before the first never-automatic
// one) and Step by step, each sent to plan-03a's
// POST /api/v1/plans/{id}/approve {plan_hash, mode, up_to} with the hash
// the person was shown. Editing is not offered. A stale hash (409
// content_changed) says the approval was voided and reloads the plan; any
// other failure keeps the controls enabled with the error. One decision is
// in flight at a time.

export const VOIDED = 'This plan changed after approval; approval voided'
const muted = { color: 'var(--text-secondary)' }
const button = { border: '1px solid var(--accent)', color: 'var(--accent)' }

function approveURL(detail, database) {
  const db = detail.database || database
  const query = db && db !== 'all' ? `?database=${encodeURIComponent(db)}` : ''
  return `/api/v1/plans/${encodeURIComponent(detail.plan.id)}/approve${query}`
}

function useDecision(detail, database, onDecided, onChanged) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const inFlight = useRef(false)
  const send = async body => {
    if (inFlight.current) return
    inFlight.current = true
    setBusy(true)
    setError('')
    try {
      const res = await postPlanDecision(approveURL(detail, database),
        { plan_hash: detail.plan_hash || detail.plan.plan_hash, ...body })
      if (res.ok) {
        onDecided()
      } else if (res.code === 'content_changed') {
        setError(VOIDED)
        onChanged()
      } else {
        setError(res.message)
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      inFlight.current = false
      setBusy(false)
    }
  }
  return { busy, error, send }
}

function NeverAutomatic({ coverage }) {
  const never = (coverage || []).filter(c => c.needs_own_approval)
  if (never.length === 0) return null
  return <ul className="text-xs list-disc pl-4" style={muted}>
    {never.map(c => <li key={c.ordinal}>Stage {c.ordinal} always needs its own approval
      {c.reason ? `: ${c.reason}` : ''}</li>)}
  </ul>
}

function UpTo({ choices, busy, send }) {
  const [upTo, setUpTo] = useState(choices[choices.length - 1])
  const chosen = choices.includes(upTo) ? upTo : choices[choices.length - 1]
  return <span className="inline-flex flex-wrap items-center gap-2">
    <label htmlFor="plan-up-to" className="text-xs" style={muted}>Approve up to stage</label>
    <select id="plan-up-to" value={chosen} disabled={busy}
      onChange={e => setUpTo(Number(e.target.value))} className="rounded text-sm px-1 py-1"
      style={{ border: '1px solid var(--border)', background: 'var(--bg-card)' }}>
      {choices.map(n => <option key={n} value={n}>{n}</option>)}
    </select>
    <button type="button" disabled={busy} aria-busy={busy} style={button}
      className="px-3 py-1.5 rounded text-sm"
      onClick={() => send({ mode: 'up_to', up_to: chosen })}>
      Approve up to stage {chosen}</button>
  </span>
}

export function PlanApproval({ detail, database, canReview = false, onDecided, onChanged }) {
  const { busy, error, send } = useDecision(detail, database, onDecided, onChanged)
  if (detail?.plan?.status !== 'proposed') return null
  const steps = sortedSteps(detail.plan)
  const choices = upToChoices(steps, detail.coverage)
  const box = children => <div data-testid="plan-approval" className="rounded border p-3
    space-y-2 text-sm" style={{ borderColor: 'var(--border)' }}>{children}</div>
  if (steps.length === 0) return box(<p style={muted}>This plan has no steps</p>)
  if (!canReview) {
    return box(<p style={muted}>Operators and admins approve plans; you can review it.</p>)
  }
  const all = steps.length === 1 ? 'Approve the stage' : `Approve all ${steps.length} stages`
  return box(<>
    <div className="flex flex-wrap items-center gap-2">
      <button type="button" disabled={busy} aria-busy={busy} style={button}
        className="px-3 py-1.5 rounded text-sm font-medium"
        onClick={() => send({ mode: 'all' })}>{all}</button>
      {choices.length > 0 && <UpTo choices={choices} busy={busy} send={send} />}
      <button type="button" disabled={busy} aria-busy={busy} style={button}
        className="px-3 py-1.5 rounded text-sm"
        onClick={() => send({ mode: 'step' })}>Approve step by step</button>
    </div>
    <NeverAutomatic coverage={detail.coverage} />
    {error && <p role="alert" className="text-sm" style={{ color: 'var(--red)' }}>{error}</p>}
  </>)
}
