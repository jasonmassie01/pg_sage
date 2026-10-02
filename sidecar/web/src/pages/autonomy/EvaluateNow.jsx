import { useState } from 'react'
import { checkText } from './checkText'

// Phase 1.1 (2026-10-02): "Evaluate now" asks pg_sage to propose what the
// evidence supports for this database and shows what it proposed and,
// for every other pair, why not. It never approves anything.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

async function evaluate(query) {
  const res = await fetch(`/api/v1/sre/autonomy/evaluate${query}`, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
  })
  if (!res.ok) {
    let message = `Evaluation failed (${res.status})`
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

export function EvaluateNow({ query, onDone }) {
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState(null)
  const [error, setError] = useState(null)
  async function run() {
    setBusy(true)
    setError(null)
    try {
      setResult(await evaluate(query))
      onDone?.()
    } catch (err) {
      setResult(null)
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="space-y-1">
      <button type="button" disabled={busy} className="rounded px-2 py-1 text-xs"
        style={{ ...strong, border: '1px solid var(--accent)' }} onClick={run}>
        Evaluate now
      </button>
      {error && <div role="alert" className="text-xs" style={{ color: 'var(--red)' }}>
        {error}</div>}
      {result && <EvaluationResult result={result} />}
    </div>
  )
}

function EvaluationResult({ result }) {
  const created = result.created || []
  const skipped = result.not_proposed || []
  const atCap = skipped.filter(p => p.reason === 'at_cap')
  const open = skipped.filter(p => p.reason !== 'at_cap')
  return (
    <div data-testid="evaluate-result" className="text-xs space-y-1" style={muted}>
      {created.length === 0
        ? <div>Evaluated: no new promotion proposed.</div>
        : <div style={strong}>Proposed {created.length} promotion(s); an admin approves
          each one under Pending promotions.</div>}
      <ul className="list-disc pl-5">
        {created.map(p => (
          <li key={p.id} style={strong}>{p.family} / {p.class}: {p.from} to {p.to}</li>
        ))}
        {open.map(p => (
          <li key={`${p.family}-${p.class}`}>
            {p.family} / {p.class}: <NotProposedReason item={p} />
          </li>
        ))}
      </ul>
      {atCap.length > 0 && (
        <div>At its cap: {atCap.map(p => `${p.family} / ${p.class}`).join(', ')}.</div>
      )}
    </div>
  )
}

function NotProposedReason({ item }) {
  if (item.reason === 'pending') return <span>a promotion already waits for an admin</span>
  return (
    <span>
      not yet {item.target}: {(item.unmet || []).map(checkText).join(' ')}
    </span>
  )
}
