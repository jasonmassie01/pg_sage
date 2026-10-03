import { useState } from 'react'

// Phase 1.1 (2026-10-02): an operator accepts or rejects a finished
// investigation's diagnosis, optionally with a note and the actual root
// cause (a causal-graph node id or free text). One request records the
// shadow review that earned autonomy counts and the investigation
// outcome in incident memory. Nothing here executes or approves anything.

const FINISHED = new Set(['concluded', 'inconclusive'])
const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const field = { border: '1px solid var(--border)', background: 'var(--bg-card)', ...strong }

function canOperate(user) {
  return user?.role === 'admin' || user?.role === 'operator'
}

async function postReview(body) {
  const res = await fetch('/api/v1/sre/autonomy/reviews', {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!res.ok) {
    let message = `Review failed (${res.status})`
    try {
      const data = await res.json()
      if (data?.error) message = data.error
    } catch {
      // keep the status as the message
    }
    throw new Error(message)
  }
  return res.json()
}

export function InvestigationReview({ database, investigation, user }) {
  const [note, setNote] = useState('')
  const [rootCause, setRootCause] = useState('')
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState(null)
  const [error, setError] = useState(null)
  if (!canOperate(user) || !FINISHED.has(investigation?.state)) return null

  // The buttons are disabled while a review is in flight (React applies
  // the disabled state before the next click event is dispatched).
  async function review(verdict) {
    setBusy(true)
    setError(null)
    try {
      setResult(await postReview({
        database, investigation_id: investigation.id, verdict,
        note: note.trim(), actual_root_cause: rootCause.trim(),
      }))
    } catch (err) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  const button = { ...strong, border: '1px solid var(--border)' }
  return (
    <div data-testid="investigation-review" className="space-y-1">
      <div className="font-medium" style={strong}>Review this diagnosis</div>
      <div style={muted}>
        Your verdict is the shadow evidence pg_sage needs before it may act on this
        family by itself; an admin still approves every promotion.
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <label style={muted}>
          Note
          <input className="ml-1 rounded px-1" style={field} value={note}
            maxLength={1500} onChange={e => setNote(e.target.value)} />
        </label>
        <label style={muted}>
          Actual root cause
          <input className="ml-1 rounded px-1" style={field} value={rootCause}
            maxLength={400} placeholder="graph node or free text"
            onChange={e => setRootCause(e.target.value)} />
        </label>
        <button type="button" disabled={busy} className="rounded px-2 py-1"
          style={{ ...button, borderColor: 'var(--green)' }}
          onClick={() => review('accepted')}>Accept diagnosis</button>
        <button type="button" disabled={busy} className="rounded px-2 py-1"
          style={{ ...button, borderColor: 'var(--red)' }}
          onClick={() => review('rejected')}>Reject diagnosis</button>
      </div>
      {error && <div role="alert" style={{ color: 'var(--red)' }}>{error}</div>}
      {result && <ReviewStatus result={result} />}
    </div>
  )
}

function ReviewStatus({ result }) {
  const outcome = result.investigation_outcome
  return (
    <div data-testid="investigation-review-status" style={muted}>
      Review recorded: {result.verdict}
      {result.family ? ` (${result.family})` : ''}
      {result.actual_node ? `, actual root ${result.actual_node}` : ''}.
      {outcome && ` Investigation outcome: ${outcome.verdict}.`}
      {result.investigation_outcome_skipped &&
        ` Investigation outcome not recorded: ${result.investigation_outcome_skipped}.`}
    </div>
  )
}
