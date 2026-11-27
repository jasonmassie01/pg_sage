import { useState } from 'react'
import { decideFact, formatFactDate } from '../../lib/facts'
import { FactCoverage } from './FactCoverage'

// FactCard shows one binding fact: the fact in words, what it is about,
// who proposed it from what evidence, its decision and provenance, and,
// for operators and admins, the decisions its status allows.

const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }
const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const badge = { border: '1px solid var(--border)', color: 'var(--text-secondary)' }

const STATUS_COLORS = {
  proposed: 'var(--yellow)', confirmed: 'var(--green)',
  rejected: 'var(--red)', expired: 'var(--text-secondary)',
}

// DECISIONS lists the buttons each status offers: [verb, label, testid].
const DECISIONS = {
  proposed: [['confirm', 'Confirm', 'confirm'], ['reject', 'Reject', 'reject']],
  confirmed: [['reject', 'Reject (revoke)', 'revoke'], ['expire', 'Expire', 'expire']],
}

export function FactCard({ fact, canDecide, onDecided }) {
  return (
    <article data-testid={`fact-card-${fact.id}`} className="rounded border p-3 space-y-2"
      style={card}>
      <div className="flex flex-wrap items-start justify-between gap-2">
        <h3 className="text-sm font-medium" style={strong}>{fact.summary}</h3>
        <div className="flex gap-1.5 text-xs">
          <span data-testid={`fact-type-${fact.id}`} className="rounded px-1.5 py-0.5"
            style={badge}>{fact.type}</span>
          <span data-testid={`fact-status-${fact.id}`} className="rounded px-1.5 py-0.5"
            style={{ ...badge, color: STATUS_COLORS[fact.status] || muted.color }}>
            {fact.status}
          </span>
        </div>
      </div>
      <FactMeta fact={fact} />
      <FactEvidence fact={fact} />
      {fact.rationale && (
        <p className="text-sm" style={strong}>Rationale: {fact.rationale}</p>
      )}
      <FactDecisionInfo fact={fact} />
      {fact.status === 'proposed' && (
        <FactCoverage database={fact.database} kind={fact.subject_kind}
          subject={fact.subject || ''} testId={`fact-coverage-${fact.id}`}
          key={`${fact.subject_kind}:${fact.subject}`} />
      )}
      {canDecide && DECISIONS[fact.status] && (
        <FactDecision fact={fact} onDecided={onDecided} />
      )}
    </article>
  )
}

function FactMeta({ fact }) {
  const value = Object.entries(fact.value || {})
  return (
    <div className="flex flex-wrap gap-x-3 gap-y-1 text-xs" style={muted}>
      {fact.database && <span>DB: {fact.database}</span>}
      <span>About: {fact.subject_kind} {fact.subject}</span>
      {value.map(([k, v]) => <span key={k}>{k}: {String(v)}</span>)}
      <span>Source: {fact.source}</span>
      {fact.proposed_by && <span>Proposed by: {fact.proposed_by}</span>}
      <span data-testid={`fact-proposals-${fact.id}`}>
        Times proposed: {fact.proposals ?? 0}
      </span>
    </div>
  )
}

function FactEvidence({ fact }) {
  const evidence = Array.isArray(fact.evidence) ? fact.evidence : []
  if (evidence.length === 0) return null
  return (
    <ul data-testid={`fact-evidence-${fact.id}`} className="text-xs space-y-0.5 list-disc ml-5"
      style={strong}>
      {evidence.map((e, i) => (
        <li key={`${e.kind}-${e.ref}-${i}`}>
          <span style={muted}>{e.kind}</span> <span className="font-mono">{e.ref}</span>
          {e.detail ? `: ${e.detail}` : ''}
        </li>
      ))}
    </ul>
  )
}

function FactDecisionInfo({ fact }) {
  const expires = formatFactDate(fact.expires_at)
  const absent = formatFactDate(fact.missing_since)
  return (
    <div className="text-xs space-y-0.5" style={muted}>
      {fact.provenance && (
        <div data-testid={`fact-provenance-${fact.id}`} style={strong}>
          {fact.provenance}
        </div>
      )}
      {fact.decision_note && <div>Note: {fact.decision_note}</div>}
      {expires && <div data-testid={`fact-expires-${fact.id}`}>Expires: {expires}</div>}
      {fact.expired_reason && <div>Expired: {fact.expired_reason}</div>}
      {absent && (
        <div data-testid={`fact-absent-${fact.id}`}>
          Absent since {absent}
          {fact.status === 'confirmed' && ' (still binds until the absence grace ends)'}
        </div>
      )}
    </div>
  )
}

// widens reports whether deciding verb lifts a guard: revoking or expiring
// a confirmed fact lets pg_sage act on its subject again (facts-07).
function widens(fact, verb) {
  return fact.status === 'confirmed' && (verb === 'reject' || verb === 'expire')
}

function FactDecision({ fact, onDecided }) {
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const [pending, setPending] = useState(null)

  async function decide(verb) {
    setPending(null)
    setBusy(true)
    setError(null)
    try {
      await decideFact(fact.database, fact.id, verb, note.trim())
      setNote('')
      onDecided?.()
    } catch (err) {
      setError(`Could not ${verb} fact #${fact.id}: ${err.message}`)
    } finally {
      setBusy(false)
    }
  }

  function choose(verb) {
    if (widens(fact, verb)) setPending(verb)
    else decide(verb)
  }

  return (
    <div className="space-y-1">
      <div className="flex flex-wrap gap-2 items-center">
        <input data-testid={`fact-note-${fact.id}`} value={note}
          onChange={e => setNote(e.target.value)} placeholder="Note (optional)"
          className="flex-1 min-w-[12rem] p-1 rounded text-sm"
          style={{ background: 'var(--bg-primary)', border: '1px solid var(--border)',
            color: 'var(--text-primary)' }} />
        {DECISIONS[fact.status].map(([verb, label, tid]) => (
          <button key={tid} type="button" data-testid={`fact-${tid}-${fact.id}`}
            disabled={busy || pending !== null} onClick={() => choose(verb)}
            className="px-3 py-1 rounded text-sm"
            style={{ border: '1px solid var(--border)', opacity: busy ? 0.5 : 1,
              color: verb === 'confirm' ? 'var(--green)' : 'var(--text-primary)' }}>
            {label}
          </button>
        ))}
      </div>
      {pending && (
        <WidenConfirm fact={fact} verb={pending} onConfirm={() => decide(pending)}
          onCancel={() => setPending(null)} />
      )}
      {error && (
        <div role="alert" data-testid={`fact-error-${fact.id}`} className="text-sm"
          style={{ color: 'var(--red)' }}>
          {error}
        </div>
      )}
    </div>
  )
}

// WidenConfirm asks before a decision that lifts a guard. Cancel sends
// nothing.
function WidenConfirm({ fact, verb, onConfirm, onCancel }) {
  const action = verb === 'expire' ? 'Expire' : 'Revoke'
  const titleId = `fact-widen-title-${fact.id}`
  return (
    <div role="alertdialog" aria-labelledby={titleId}
      data-testid={`fact-widen-${fact.id}`} className="rounded border p-2 space-y-2 text-sm"
      style={{ borderColor: 'var(--yellow)', background: 'var(--bg-primary)' }}>
      <div id={titleId} className="font-medium" style={strong}>
        {action} fact #{fact.id}: {fact.summary}?
      </div>
      <p style={muted}>
        It stops binding. pg_sage and operator approvals may then act on {fact.subject} again.
      </p>
      <div className="flex gap-2">
        <button type="button" data-testid={`fact-widen-confirm-${fact.id}`}
          onClick={onConfirm} className="px-3 py-1 rounded text-sm"
          style={{ border: '1px solid var(--border)', color: 'var(--red)' }}>
          {action}
        </button>
        <button type="button" data-testid={`fact-widen-cancel-${fact.id}`}
          onClick={onCancel} className="px-3 py-1 rounded text-sm"
          style={{ border: '1px solid var(--border)', color: 'var(--text-primary)' }}>
          Cancel
        </button>
      </div>
    </div>
  )
}
