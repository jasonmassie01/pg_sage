/* eslint-disable react-refresh/only-export-components */
import { useState } from 'react'
import { SQLBlock } from '../../components/SQLBlock'
import { useToast } from '../../components/Toast'
import { ShadowHistory } from './ShadowHistory'

// ApprovalCard shows one queued action with the why (roadmap 1.5): what
// pg_sage wants to do, why it needs a person, the cited evidence, the
// model's rationale, the predicted effect, the exact SQL and rollback,
// the blast radius, and Approve / Reject (reason) / Snooze. Approval is
// bound to the card hash: if the action changed since the card was
// loaded, the server refuses it.

const RISK_COLORS = {
  safe: 'var(--green)', moderate: 'var(--yellow)', high: 'var(--red)',
}

const SNOOZE_HOURS = [1, 4, 24, 72]

function formatBytes(n) {
  if (!Number.isFinite(n)) return null
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = n
  let i = 0
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024
    i++
  }
  return i === 0 ? `${value} B` : `${value.toFixed(1)} ${units[i]}`
}

function expiresIn(expiresAt, now) {
  if (!expiresAt) return 'No expiry'
  const ms = new Date(expiresAt).getTime() - now
  if (Number.isNaN(ms)) return 'No expiry'
  if (ms <= 0) return 'Expired'
  const hours = Math.floor(ms / 3600000)
  if (hours < 1) return `Expires in ${Math.floor(ms / 60000)}m`
  return `Expires in ${hours}h`
}

// approvalOutcomeMessage turns an approve response into a truthful
// message: executed, approved but not run, or refused (and why).
export function approvalOutcomeMessage(status, json) {
  const body = json || {}
  if (status >= 200 && status < 300 && body.executed) {
    return { ok: true, text: 'Approved and executed; verification: ' +
      (body.verification_status || 'pending') }
  }
  if (status >= 200 && status < 300) {
    return { ok: false, text: 'Approved, but the action did not run: ' +
      (body.error || 'unknown error') }
  }
  if (status === 409 && body.code === 'content_changed') {
    return { ok: false, text: 'This action changed after this card was shown. ' +
      'Reload and review it again.' }
  }
  return { ok: false, text: body.error || `Approve failed (${status})` }
}

function Section({ title, testId, children }) {
  return (
    <div data-testid={testId}>
      <div className="text-xs font-medium mb-1" style={{ color: 'var(--text-secondary)' }}>
        {title}
      </div>
      {children}
    </div>
  )
}

function WhySection({ why }) {
  const reasons = Array.isArray(why) && why.length > 0 ? why
    : [{ code: 'queued_for_approval', text: 'The policy queued this change for approval' }]
  return (
    <div data-testid="approval-why" className="p-2 rounded"
      style={{ border: '1px solid var(--yellow)' }}>
      <div className="text-sm font-medium mb-1" style={{ color: 'var(--yellow)' }}>
        Why it needs you
      </div>
      <ul className="list-disc ml-5 text-sm" style={{ color: 'var(--text-primary)' }}>
        {reasons.map(r => <li key={r.code + r.text}>{r.text}</li>)}
      </ul>
    </div>
  )
}

// TrustLine is the action class's trust on the database (roadmap 1.2):
// its level, evidence counts and path to the next level, in one line.
function TrustLine({ trust }) {
  if (!trust || !trust.line) return null
  return (
    <div data-testid="approval-trust" className="text-sm"
      style={{ color: trust.unavailable ? 'var(--yellow)' : 'var(--text-secondary)' }}>
      {trust.line}{' '}
      <a href="#/trust" className="underline">Trust page</a>
    </div>
  )
}

function EvidenceSection({ evidence }) {
  if (!Array.isArray(evidence) || evidence.length === 0) return null
  return (
    <Section title="Evidence" testId="approval-evidence">
      <ul className="text-sm space-y-0.5" style={{ color: 'var(--text-primary)' }}>
        {evidence.map((e, i) => (
          <li key={`${e.ref}-${e.label}-${i}`}>
            <span className="font-medium">{e.label}</span>
            {e.value ? `: ${e.value}` : ''}
            <span className="ml-2 text-xs" style={{ color: 'var(--text-secondary)' }}>
              {e.ref}
            </span>
          </li>
        ))}
      </ul>
    </Section>
  )
}

// confidenceLabel shows the tuning agent's calibration (roadmap 2.2) and
// otherwise a producer's own confidence; an uncalibrated proposal shows
// no number.
function confidenceLabel(rationale) {
  const known = Number.isFinite(rationale.confidence)
  const value = known ? Math.round(rationale.confidence * 100) : null
  if (rationale.calibration && known) {
    return ` (calibrated confidence ${value}%: ${rationale.calibration})`
  }
  if (rationale.calibration) return ` (${rationale.calibration})`
  return known ? ` (confidence ${value}%)` : ''
}

function RationaleSection({ rationale }) {
  if (!rationale || !rationale.text) return null
  const source = rationale.source === 'llm' ? 'Model rationale' : 'Rationale'
  return (
    <Section title={source + confidenceLabel(rationale)} testId="approval-rationale">
      <p className="text-sm" style={{ color: 'var(--text-primary)' }}>{rationale.text}</p>
    </Section>
  )
}

function PredictedSection({ predicted }) {
  const p = predicted || {}
  const parts = []
  if (Number.isFinite(p.improvement_pct)) {
    const queries = (p.affected_queries || []).length
    parts.push(`${p.improvement_pct.toFixed(1)}% faster` +
      (queries ? ` on ${queries} quer${queries === 1 ? 'y' : 'ies'}` : ''))
  }
  if (Number.isFinite(p.estimated_size_bytes)) {
    parts.push(`about ${formatBytes(p.estimated_size_bytes)}`)
  }
  if (p.what_if_verdict) parts.push(`HypoPG what-if: ${p.what_if_verdict}`)
  if (parts.length === 0) return null
  return (
    <Section title="Predicted effect" testId="approval-predicted">
      <p className="text-sm" style={{ color: 'var(--text-primary)' }}>
        {parts.join(' · ')}
        {p.method === 'llm_estimate' && ' (estimate, not measured)'}
      </p>
    </Section>
  )
}

function RiskSection({ risk }) {
  const r = risk || {}
  return (
    <Section title="Risk and blast radius" testId="approval-risk">
      <div className="text-sm space-y-0.5" style={{ color: 'var(--text-primary)' }}>
        <div>
          Risk: <span style={{ color: RISK_COLORS[r.tier] || 'var(--text-secondary)' }}>
            {r.tier || 'unknown'}</span>
        </div>
        {r.blast_radius && <div>Blast radius: {r.blast_radius}</div>}
        {r.lock && <div>Lock: {r.lock}</div>}
        {(r.guardrails || []).length > 0 && (
          <div>Guardrails: {r.guardrails.join(', ')}</div>
        )}
      </div>
    </Section>
  )
}

function RollbackSection({ rollback }) {
  const r = rollback || {}
  const title = r.class ? `Rollback (${r.class.replaceAll('_', ' ')})` : 'Rollback'
  return (
    <Section title={title} testId="approval-rollback">
      {r.sql ? <SQLBlock sql={r.sql} />
        : <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>{r.note}</p>}
    </Section>
  )
}

// facts is an optional node (the binding facts about the card's targets)
// shown under the why and trust lines.
export function ApprovalCard({ card, onDecided, facts = null }) {
  const [now] = useState(() => Date.now())
  return (
    <div data-testid="approval-card" className="rounded p-4 space-y-3"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div data-testid="approval-card-title" className="font-medium"
          style={{ color: 'var(--text-primary)' }}>{card.title}</div>
        <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          {card.database} · queue item {card.queue_id} ·{' '}
          <span data-testid="approval-expiry">{expiresIn(card.expires_at, now)}</span>
        </div>
      </div>
      {card.snoozed_until && (
        <div data-testid="approval-snoozed-badge" className="text-xs"
          style={{ color: 'var(--text-secondary)' }}>
          Snoozed until {new Date(card.snoozed_until).toLocaleString()}
          {card.snooze_reason ? ` — ${card.snooze_reason}` : ''}
        </div>
      )}
      <WhySection why={card.why_approval} />
      <TrustLine trust={card.trust} />
      {facts}
      <EvidenceSection evidence={card.evidence} />
      <RationaleSection rationale={card.rationale} />
      <PredictedSection predicted={card.predicted_effect} />
      <Section title="SQL" testId="approval-sql"><SQLBlock sql={card.sql} /></Section>
      <RollbackSection rollback={card.rollback} />
      <RiskSection risk={card.risk} />
      <ShadowHistory history={card.shadow_history} />
      <ApprovalActions card={card} onDecided={onDecided} />
    </div>
  )
}

function decisionURL(card, verb) {
  const db = card.database ? `?database=${encodeURIComponent(card.database)}` : ''
  return `/api/v1/approvals/${card.queue_id}/${verb}${db}`
}

async function postDecision(card, verb, body) {
  const res = await fetch(decisionURL(card, verb), {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
  })
  let json = {}
  try {
    json = await res.json()
  } catch (err) {
    json = { error: `unreadable response (${err.message})` }
  }
  return { status: res.status, json }
}

function ApprovalActions({ card, onDecided }) {
  const toast = useToast()
  const [busy, setBusy] = useState(false)
  const [mode, setMode] = useState(null)
  const [reason, setReason] = useState('')
  const [hours, setHours] = useState(4)
  const [snoozeReason, setSnoozeReason] = useState('')

  async function run(verb, body, describe) {
    setBusy(true)
    try {
      const { status, json } = await postDecision(card, verb, body)
      const msg = describe(status, json)
      if (msg.ok) toast.success(msg.text)
      else toast.error(msg.text)
    } catch (err) {
      toast.error(`Request failed: ${err.message}`)
    } finally {
      setBusy(false)
      setMode(null)
      onDecided?.()
    }
  }

  const simple = (done) => (status, json) => (status >= 200 && status < 300
    ? { ok: true, text: done } : { ok: false, text: json.error || `Failed (${status})` })

  return (
    <div data-testid="approval-actions" className="space-y-2">
      <div className="flex flex-wrap gap-2">
        <button data-testid="approval-approve" disabled={busy}
          onClick={() => run('approve', { card_hash: card.card_hash },
            approvalOutcomeMessage)}
          className="px-3 py-1 rounded text-sm"
          style={{ background: 'var(--green)', color: '#fff', opacity: busy ? 0.5 : 1 }}>
          Approve
        </button>
        <button data-testid="approval-reject" disabled={busy}
          onClick={() => setMode(mode === 'reject' ? null : 'reject')}
          className="px-3 py-1 rounded text-sm"
          style={{ background: 'var(--red)', color: '#fff', opacity: busy ? 0.5 : 1 }}>
          Reject
        </button>
        <button data-testid="approval-snooze" disabled={busy}
          onClick={() => setMode(mode === 'snooze' ? null : 'snooze')}
          className="px-3 py-1 rounded text-sm"
          style={{ border: '1px solid var(--border)', color: 'var(--text-primary)' }}>
          Snooze
        </button>
      </div>
      {mode === 'reject' && (
        <div className="flex flex-wrap gap-2 items-start">
          <textarea data-testid="approval-reject-reason" value={reason}
            onChange={e => setReason(e.target.value)} rows={2}
            placeholder="Why not? (kept with the action; pg_sage will not run it unasked)"
            className="flex-1 min-w-[16rem] p-2 rounded text-sm"
            style={{ background: 'var(--bg-primary)', border: '1px solid var(--border)',
              color: 'var(--text-primary)' }} />
          <button data-testid="approval-reject-confirm" disabled={busy || !reason.trim()}
            onClick={() => run('reject', { reason: reason.trim(),
              card_hash: card.card_hash }, simple(`Action ${card.queue_id} rejected`))}
            className="px-3 py-1 rounded text-sm"
            style={{ background: 'var(--red)', color: '#fff',
              opacity: busy || !reason.trim() ? 0.5 : 1 }}>
            Reject with reason
          </button>
        </div>
      )}
      {mode === 'snooze' && (
        <div className="flex flex-wrap gap-2 items-center">
          <select data-testid="approval-snooze-hours" value={hours}
            onChange={e => setHours(Number(e.target.value))}
            className="p-1 rounded text-sm"
            style={{ background: 'var(--bg-primary)', border: '1px solid var(--border)',
              color: 'var(--text-primary)' }}>
            {SNOOZE_HOURS.map(h => <option key={h} value={h}>{h}h</option>)}
          </select>
          <input data-testid="approval-snooze-reason" value={snoozeReason}
            onChange={e => setSnoozeReason(e.target.value)} placeholder="Reason (optional)"
            className="flex-1 min-w-[12rem] p-1 rounded text-sm"
            style={{ background: 'var(--bg-primary)', border: '1px solid var(--border)',
              color: 'var(--text-primary)' }} />
          <button data-testid="approval-snooze-confirm" disabled={busy}
            onClick={() => run('snooze', { hours, reason: snoozeReason.trim() },
              simple(`Snoozed for ${hours}h`))}
            className="px-3 py-1 rounded text-sm"
            style={{ border: '1px solid var(--border)', color: 'var(--text-primary)' }}>
            Snooze {hours}h
          </button>
        </div>
      )}
    </div>
  )
}
