import { useState } from 'react'
import { canDecideReviews, dbaVerdictLabel, formatTime, reviewVerdictURL } from
  '../../lib/reviews'

// VerdictForm records the DBA verdict on a complete review: concur,
// concur with edits, or override with a reason and a direction (too
// strict or too lenient). Only operators and admins see the buttons; the
// API refuses everyone else too. A verdict is recorded once: a second one
// answers 409 naming who decided first.

const CHOICES = [
  { verdict: 'concur', label: 'Concur', tid: 'verdict-concur' },
  { verdict: 'concur_with_edits', label: 'Concur with edits',
    tid: 'verdict-concur-with-edits' },
  { verdict: 'override', label: 'Override', tid: 'verdict-override' },
]

const button = { color: 'var(--text-primary)', border: '1px solid var(--border)' }

async function postVerdict(url, body) {
  const res = await fetch(url, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (res.status === 409) {
    throw new Error(`already decided by ${data.decided_by || 'another DBA'}`)
  }
  if (!res.ok) throw new Error(data.error || `Request failed (${res.status})`)
  return data
}

function verdictBody(choice, reason, direction) {
  const body = { verdict: choice }
  if (choice === 'override') body.direction = direction
  if (reason.trim()) body.reason = reason.trim()
  return body
}

function Decided({ review }) {
  return (
    <div data-testid="verdict-decided" className="text-sm"
      style={{ color: 'var(--text-primary)' }}>
      DBA verdict: {dbaVerdictLabel(review)}
      {review.dba_actor ? ` by ${review.dba_actor}` : ''}
      {review.dba_decided_at ? ` at ${formatTime(review.dba_decided_at)}` : ''}
      {review.dba_reason && (
        <div className="text-xs mt-1" style={{ color: 'var(--text-secondary)' }}>
          Reason: {review.dba_reason}
        </div>
      )}
      {review.verdict_carried_from && (
        <div className="text-xs mt-1" style={{ color: 'var(--text-secondary)' }}>
          Carried over from revision {review.verdict_carried_from.slice(0, 8)} (same
          statements and database-relevant hunks).
        </div>
      )}
    </div>
  )
}

function ChoiceForm({ choice, onCancel, onSubmit, busy, error }) {
  const [reason, setReason] = useState('')
  const [direction, setDirection] = useState('')
  const override = choice === 'override'
  const ready = !busy && (!override || (reason.trim() !== '' && direction !== ''))
  return (
    <div className="mt-2 space-y-2">
      <textarea data-testid="verdict-reason" value={reason} rows={2}
        onChange={e => setReason(e.target.value)}
        placeholder={override ? 'Why pg_sage was wrong (required)' : 'Note (optional)'}
        className="w-full rounded p-2 text-sm"
        style={{ ...button, background: 'var(--bg-card)' }} />
      {override && (
        <select data-testid="verdict-direction" value={direction}
          onChange={e => setDirection(e.target.value)}
          aria-label="Override direction" className="rounded p-1 text-sm"
          style={{ ...button, background: 'var(--bg-card)' }}>
          <option value="">Direction…</option>
          <option value="too_strict">too strict</option>
          <option value="too_lenient">too lenient</option>
        </select>
      )}
      <div className="flex gap-2">
        <button type="button" data-testid="verdict-submit" disabled={!ready}
          onClick={() => onSubmit(verdictBody(choice, reason, direction))}
          className="rounded px-2.5 py-1 text-xs" style={button}>
          Submit
        </button>
        <button type="button" data-testid="verdict-cancel" onClick={onCancel}
          className="rounded px-2.5 py-1 text-xs" style={button}>
          Cancel
        </button>
      </div>
      {error && (
        <p data-testid="verdict-error" role="alert" className="text-sm"
          style={{ color: 'var(--red)' }}>
          {error}
        </p>
      )}
    </div>
  )
}

export function VerdictForm({ review, user, onDecided }) {
  const [choice, setChoice] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  if (review.dba_verdict) return <Decided review={review} />
  if (review.state !== 'complete') return null
  if (!canDecideReviews(user)) {
    return (
      <p data-testid="verdict-viewer-note" className="text-sm"
        style={{ color: 'var(--text-secondary)' }}>
        Awaiting a DBA verdict. Only operators and admins record verdicts.
      </p>
    )
  }
  async function submit(body) {
    setBusy(true)
    setError('')
    try {
      const decided = await postVerdict(reviewVerdictURL(review.database, review.id), body)
      if (onDecided) onDecided(decided)
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div>
      <div className="flex flex-wrap gap-2" aria-label="DBA verdict">
        {CHOICES.map(c => (
          <button key={c.verdict} type="button" data-testid={c.tid}
            aria-pressed={choice === c.verdict}
            onClick={() => { setChoice(c.verdict); setError('') }}
            className="rounded px-2.5 py-1 text-xs" style={button}>
            {c.label}
          </button>
        ))}
      </div>
      {choice && (
        <ChoiceForm key={choice} choice={choice} busy={busy} error={error}
          onCancel={() => setChoice('')} onSubmit={submit} />
      )}
    </div>
  )
}
