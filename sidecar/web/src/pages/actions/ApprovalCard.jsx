/* eslint-disable react-refresh/only-export-components */
import { useState } from 'react'
import { SQLBlock } from '../../components/SQLBlock'
import { HintPlanSection } from './HintPlanSection'
import { LadderSection } from './LadderSection'
import { ShadowHistory } from './ShadowHistory'
import { riskLabel } from './workflow'
import { ApprovalActions } from './ApprovalActions'
import { formatTime } from '../../lib/time'

export { approvalOutcomeMessage } from './approvalOutcome'

// ApprovalCard shows one queued action with the why (roadmap 1.5): what
// pg_sage wants to do, why it needs a person, the cited evidence, the
// model's rationale, the predicted effect, the exact SQL and rollback,
// the blast radius, and Approve / Reject (reason) / Snooze. Approval is
// bound to the card hash: if the action changed since the card was
// loaded, the server refuses it.

const RISK_COLORS = {
  safe: 'var(--green)', moderate: 'var(--yellow)', high: 'var(--red)', high_risk: 'var(--red)',
}

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

// WaitLine is another change still being verified on this change's object
// (one change per object): approving overrides that verification.
function WaitLine({ wait }) {
  if (!wait || !wait.line) return null
  return (
    <div data-testid="approval-verification-wait" className="text-sm"
      style={{ color: 'var(--yellow)' }}>
      {wait.line}
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
    parts.push(`${p.improvement_pct.toFixed(1)}% estimated improvement` +
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
        {p.method === 'llm_estimate' ? ' (model estimate; not measured)'
          : p.method === 'hypopg' ? ' (planner estimate; runtime improvement has not been measured)'
            : ' (estimate; method unavailable, not measured)'}
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
            {riskLabel(r.tier)}</span>
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
        : <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>{r.note || 'A recovery procedure has not been supplied.'}</p>}
    </Section>
  )
}

// facts is an optional node (the binding facts about the card's targets)
// shown under the why and trust lines.
// surface names the page deciding (X-Sage-Surface, ux-01a); the Inbox passes 'inbox'.
// actionsRef receives the decision verbs the approval list's keys call (ux-05a).
export function ApprovalCard({ card, onDecided, facts = null, eligible, blockedReason, mode,
  windowOnly = false, surface, actionsRef }) {
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
      {card.origin && (
        <div data-testid="approval-origin" className="text-xs"
          style={{ color: 'var(--text-secondary)' }}>
          {`Proposed via ${card.origin.label || card.origin.via} by ${card.origin.by}`}
        </div>
      )}
      {card.snoozed_until && (
        <div data-testid="approval-snoozed-badge" className="text-xs"
          style={{ color: 'var(--text-secondary)' }}>
          Snoozed until {formatTime(card.snoozed_until)}
          {card.snooze_reason ? ` — ${card.snooze_reason}` : ''}
        </div>
      )}
      <WhySection why={card.why_approval} />
      <TrustLine trust={card.trust} />
      <WaitLine wait={card.verification_wait} />
      {facts}
      <EvidenceSection evidence={card.evidence} />
      <HintPlanSection evidence={card.hint_evidence} />
      <LadderSection ladder={card.ladder} note={card.ladder_note} />
      <RationaleSection rationale={card.rationale} />
      <PredictedSection predicted={card.predicted_effect} />
      <Section title="SQL" testId="approval-sql"><SQLBlock sql={card.sql} /></Section>
      <RollbackSection rollback={card.rollback} />
      <RiskSection risk={card.risk} />
      <Section title="How the outcome will be checked" testId="approval-verification-plan">
        <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          {card.risk?.post_checks?.length ? card.risk.post_checks.join('; ')
            : 'An outcome-checking plan has not been supplied in this card.'}
        </p>
      </Section>
      <ShadowHistory history={card.shadow_history} />
      <ApprovalActions card={card} onDecided={onDecided} surface={surface}
        eligible={eligible} blockedReason={blockedReason} mode={mode} windowOnly={windowOnly}
        actionsRef={actionsRef} />
    </div>
  )
}
