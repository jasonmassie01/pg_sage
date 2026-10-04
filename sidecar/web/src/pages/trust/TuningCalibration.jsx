import { useEffect, useState } from 'react'

// Roadmap 2.2: the tuning agent's calibrated confidence (GET
// /api/v1/tuning/calibration). Per action class and prediction method,
// the reliability bins map the predicted improvement to what the outcome
// ledger observed. Below the minimum number of decided outcomes a class
// is "uncalibrated" and no confidence is shown.

const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }
const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

const pct = v => `${Math.round(v)}%`

function BinCell({ b }) {
  return (
    <li>
      {b.label}%: {b.hits}/{b.n}
      {Number.isFinite(b.mean_predicted) && Number.isFinite(b.mean_observed) && (
        <span style={muted}>
          {' '}(predicted {pct(b.mean_predicted)}, observed {pct(b.mean_observed)})
        </span>
      )}
    </li>
  )
}

function ClassRow({ c, minOutcomes }) {
  const calibrated = c.status === 'calibrated'
  const bins = (c.bins || []).filter(b => b.n > 0)
  return (
    <tr data-testid={`tuning-calibration-${c.class}-${c.method}`}
      style={{ borderTop: '1px solid var(--border)' }}>
      <td className="py-1 pr-3" style={strong}>{c.class}</td>
      <td className="py-1 pr-3">{c.method}</td>
      <td className="py-1 pr-3">
        {calibrated
          ? `${c.hits} of ${c.n} improved`
          : `uncalibrated (${c.n} of ${minOutcomes} outcomes)`}
      </td>
      <td className="py-1">
        {calibrated && bins.length > 0 && (
          <ul className="text-xs">{bins.map(b => <BinCell key={b.label} b={b} />)}</ul>
        )}
      </td>
    </tr>
  )
}

async function loadCalibration(database) {
  const res = await fetch(
    `/api/v1/tuning/calibration?database=${encodeURIComponent(database)}`,
    { credentials: 'include' })
  let body = {}
  try {
    body = await res.json()
  } catch {
    // keep the status as the message
  }
  if (!res.ok) throw new Error(body.error || `${res.status}`)
  return body
}

export function TuningCalibration({ database }) {
  const one = database && database !== 'all'
  const [state, setState] = useState({ loading: one, data: null, error: null })
  useEffect(() => {
    if (!one) return undefined
    let live = true
    loadCalibration(database)
      .then(data => live && setState({ loading: false, data, error: null }))
      .catch(err => live && setState({ loading: false, data: null, error: err.message }))
    return () => { live = false }
  }, [database, one])

  const classes = state.data?.classes || []
  const minOutcomes = state.data?.min_outcomes
  return (
    <section className="rounded border p-3 space-y-2 text-sm" style={card}
      data-testid="tuning-calibration">
      <h3 className="text-sm font-semibold" style={strong}>
        Tuning agent calibration (predicted vs observed)
      </h3>
      {!one && (
        <div data-testid="tuning-calibration-pick" style={muted}>
          Pick one database: each database has its own outcome ledger.
        </div>
      )}
      {state.loading && <div style={muted}>Loading…</div>}
      {state.error && (
        <div role="alert" style={{ color: 'var(--red)' }}>
          Could not load the tuning calibration: {state.error}
        </div>
      )}
      {one && !state.loading && !state.error && classes.length === 0 && (
        <div data-testid="tuning-calibration-empty" style={muted}>
          No decided outcomes yet: every tuning proposal is uncalibrated and shows no
          confidence.
        </div>
      )}
      {classes.length > 0 && (
        <table className="w-full text-left">
          <thead>
            <tr style={muted}>
              <th className="pr-3 font-normal">action</th>
              <th className="pr-3 font-normal">prediction</th>
              <th className="pr-3 font-normal">outcomes</th>
              <th className="font-normal">by predicted improvement</th>
            </tr>
          </thead>
          <tbody>
            {classes.map(c => (
              <ClassRow key={`${c.class}-${c.method}`} c={c} minOutcomes={minOutcomes} />
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}
