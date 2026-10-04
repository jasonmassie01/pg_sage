import { useState } from 'react'

// Shadow mode (roadmap 1.4): below a class's earned level pg_sage records
// every action it would have taken and scores it later from what
// happened. Per class: how many, how they scored, and on demand what
// pg_sage would have done.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

const SCORE_COLORS = {
  correct: 'var(--green)', incorrect: 'var(--red)', neutral: 'var(--text-secondary)',
  unscored: 'var(--text-secondary)',
}

export function ShadowSummary({ summary }) {
  if (!summary || !summary.total) return null
  const parts = [
    `${summary.correct || 0} correct`,
    `${summary.incorrect || 0} incorrect`,
    `${summary.neutral || 0} neutral`,
    `${summary.unscored || 0} unscored`,
    `${summary.pending || 0} pending`,
  ]
  return (
    <div data-testid="trust-shadow" style={muted}>
      <span style={strong}>{summary.total} shadow decisions</span>: {parts.join(' · ')}
      {' '}({summary.counted || 0} counted as shadow evidence)
    </div>
  )
}

function prediction(p) {
  if (!p || !p.metric) return 'no prediction'
  const pct = Number.isFinite(p.expected_change_pct)
    ? ` ${p.expected_change_pct > 0 ? '+' : ''}${p.expected_change_pct}%` : ''
  return `${p.metric}${pct} (${p.method || 'none'})`
}

function ShadowItem({ d }) {
  const score = d.status === 'scored' ? d.score : 'pending'
  return (
    <li data-testid={`shadow-decision-${d.id}`} className="space-y-0.5 py-1"
      style={{ borderTop: '1px solid var(--border)' }}>
      <code className="block whitespace-pre-wrap break-all" style={strong}>{d.sql}</code>
      {d.rollback_sql && <div style={muted}>rollback: <code>{d.rollback_sql}</code></div>}
      <div style={muted}>
        predicted {prediction(d.prediction)} · gate: {d.gate_verdict} ({d.gate_reason})
        {' '}· if trusted: {d.trusted_verdict}
        {d.trusted_reason ? ` (${d.trusted_reason})` : ''}
      </div>
      <div>
        <span style={{ color: SCORE_COLORS[score] || 'var(--text-secondary)' }}>{score}</span>
        {d.score_source && <span style={muted}> · {d.score_source}</span>}
        {d.counted && <span style={muted}> · counted</span>}
        {d.score_reason && <span style={muted}> · {d.score_reason}</span>}
      </div>
    </li>
  )
}

async function loadDecisions(database, cls) {
  const url = `/api/v1/shadow-decisions?database=${encodeURIComponent(database)}`
    + `&class=${encodeURIComponent(cls)}&limit=20`
  const res = await fetch(url, { credentials: 'include' })
  let body = {}
  try {
    body = await res.json()
  } catch {
    // keep the status as the message
  }
  if (!res.ok) throw new Error(body.error || `${res.status}`)
  const db = (body.databases || [])[0]
  return db?.decisions || []
}

export function ShadowDecisions({ database, cls }) {
  const [state, setState] = useState({ open: false, loading: false, items: null,
    error: null })
  const toggle = async () => {
    if (state.open) {
      setState(s => ({ ...s, open: false }))
      return
    }
    setState({ open: true, loading: true, items: null, error: null })
    try {
      const items = await loadDecisions(database, cls)
      setState({ open: true, loading: false, items, error: null })
    } catch (err) {
      setState({ open: true, loading: false, items: null, error: err.message })
    }
  }
  return (
    <div>
      <button type="button" className="underline" style={muted} onClick={toggle}>
        {state.open ? 'Hide' : 'What pg_sage would have done'}
      </button>
      {state.open && state.loading && <div style={muted}>Loading…</div>}
      {state.open && state.error && (
        <div role="alert" style={{ color: 'var(--red)' }}>{state.error}</div>
      )}
      {state.open && state.items && state.items.length === 0 && (
        <div style={muted}>No shadow decisions for this class yet.</div>
      )}
      {state.open && state.items && state.items.length > 0 && (
        <ul className="mt-1">
          {state.items.map(d => <ShadowItem key={d.id} d={d} />)}
        </ul>
      )}
    </div>
  )
}
