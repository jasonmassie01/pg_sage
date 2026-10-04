import { useEffect, useState } from 'react'

// Roadmap 2.4 (measure the model): "model lift over deterministic, per
// family" from the newest held-out bench measurement (GET
// /api/v1/model-lift): the model's Safe Pass against the causal graph's,
// its override precision with the Wilson lower bound the rule reads, the
// inconclusive-case lift, and whether the model may override the graph's
// root for the family or its roots stay advisory (L1), with the reason.

const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }
const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

const frac = m => (m && m.n ? `${m.k}/${m.n}` : 'n/a')

function points(a, b) {
  if (!a?.n || !b?.n) return 'n/a'
  const v = Math.round((a.k / a.n - b.k / b.n) * 1000) / 10
  return `${v > 0 ? '+' : ''}${v} pts`
}

function signed(n) {
  return `${n > 0 ? '+' : ''}${n}`
}

function LiftRow({ a }) {
  const l = a.lift
  const lb = a.rule?.lower_bound
  return (
    <tr data-testid={`model-lift-${a.family}`} style={{ borderTop: '1px solid var(--border)' }}>
      <td className="py-1 pr-3" style={strong}>{a.family}</td>
      {l ? (
        <>
          <td className="py-1 pr-3">
            {frac(l.safe_pass)} vs {frac(l.baseline_safe_pass)}{' '}
            <span style={muted}>({points(l.safe_pass, l.baseline_safe_pass)})</span>
          </td>
          <td className="py-1 pr-3">
            {frac(l.override_precision)}
            {lb != null && <span style={muted}> (lower bound {lb.toFixed(2)})</span>}
          </td>
          <td className="py-1 pr-3">
            {signed((l.inconclusive_resolved_right || 0) - (l.inconclusive_resolved_wrong || 0))}
            <span style={muted}>
              {' '}({l.inconclusive_resolved_right || 0} right, {l.inconclusive_resolved_wrong || 0}
              {' '}wrong of {l.inconclusive_runs || 0})
            </span>
          </td>
        </>
      ) : (
        <td className="py-1 pr-3" colSpan={3} style={muted}>not measured</td>
      )}
      <td className="py-1" data-testid="model-lift-authority">
        <span style={{ color: a.granted ? 'var(--green)' : 'var(--text-secondary)' }}>
          {a.granted ? 'model may override the graph' : 'advisory (L1)'}
        </span>
        <div className="text-xs" style={muted}>{a.reason}</div>
      </td>
    </tr>
  )
}

async function loadLift(database) {
  const query = database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}` : ''
  const res = await fetch(`/api/v1/model-lift${query}`, { credentials: 'include' })
  let body = {}
  try {
    body = await res.json()
  } catch {
    // keep the status as the message
  }
  if (!res.ok) throw new Error(body.error || `${res.status}`)
  return body
}

export function ModelLift({ database }) {
  const [state, setState] = useState({ loading: true, data: null, error: null })
  useEffect(() => {
    let live = true
    loadLift(database)
      .then(data => live && setState({ loading: false, data, error: null }))
      .catch(err => live && setState({ loading: false, data: null, error: err.message }))
    return () => { live = false }
  }, [database])

  const families = state.data?.families || []
  const measured = families.filter(f => f.lift)
  const report = measured.find(f => f.report)?.report
  return (
    <section className="rounded border p-3 space-y-2 text-sm" style={card}
      data-testid="model-lift">
      <h3 className="text-sm font-semibold" style={strong}>
        Model lift over deterministic (held-out bench)
      </h3>
      {state.data?.meaning && <p className="text-xs" style={muted}>{state.data.meaning}</p>}
      {state.loading && <div style={muted}>Loading…</div>}
      {state.error && <div role="alert" style={{ color: 'var(--red)' }}>{state.error}</div>}
      {!state.loading && !state.error && measured.length === 0 && (
        <div data-testid="model-lift-empty" style={muted}>
          No held-out live-model measurement yet: model-sourced roots stay advisory (L1)
          for every family.
        </div>
      )}
      {report && (
        <div className="text-xs" style={muted}>
          From {report.provenance}, generated {new Date(report.generated_at).toLocaleString()}.
        </div>
      )}
      {measured.length > 0 && (
        <table className="w-full text-left">
          <thead>
            <tr style={muted}>
              <th className="pr-3 font-normal">family</th>
              <th className="pr-3 font-normal">Safe Pass (model vs graph)</th>
              <th className="pr-3 font-normal">override precision</th>
              <th className="pr-3 font-normal">inconclusive lift</th>
              <th className="font-normal">root authority</th>
            </tr>
          </thead>
          <tbody>
            {families.map(a => <LiftRow key={a.family} a={a} />)}
          </tbody>
        </table>
      )}
    </section>
  )
}
