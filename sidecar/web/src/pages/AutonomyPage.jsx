import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'

// Sage SRE M7 earned autonomy: the level pg_sage has earned per incident
// family x action class, the evidence behind it, pending promotions
// (pg_sage proposes, an admin approves), downgrades and history.

const LEVELS = ['L0', 'L1', 'L2', 'L3']

function dbQuery(database, extra = '') {
  const params = new URLSearchParams(extra)
  if (database && database !== 'all') params.set('database', database)
  const qs = params.toString()
  return qs ? `?${qs}` : ''
}

const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }
const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

async function postJSON(url, body) {
  const res = await fetch(url, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!res.ok) {
    let message = `${res.status}`
    try {
      const data = await res.json()
      message = data.error || message
    } catch {
      // keep the status as the message
    }
    throw new Error(message)
  }
  return res.json()
}

export function AutonomyPage({ database, user }) {
  const q = dbQuery(database)
  const view = useAPI(`/api/v1/sre/autonomy${q}`, 30000)
  const history = useAPI(`/api/v1/sre/autonomy/history${dbQuery(database, 'limit=50')}`,
    30000)
  const gameDays = useAPI(`/api/v1/sre/autonomy/game-days${q}`, 60000)
  const rollouts = useAPI('/api/v1/sre/autonomy/rollouts', 60000)
  const [actionError, setActionError] = useState(null)

  if (view.loading) return <LoadingSpinner />
  if (view.error) return <ErrorBanner message={view.error} onRetry={view.refetch} />

  const role = user?.role
  const canAct = role === 'admin' || role === 'operator'
  const run = async (url, body) => {
    setActionError(null)
    try {
      await postJSON(url, body)
      view.refetch?.()
      history.refetch?.()
    } catch (err) {
      setActionError(err.message)
    }
  }
  const families = view.data?.view?.families || []
  return (
    <section className="space-y-4" data-testid="autonomy-page">
      <Header data={view.data} />
      {actionError && (
        <div role="alert" className="text-sm" style={{ color: 'var(--red)' }}>
          {actionError}
        </div>
      )}
      <PendingProposals families={families} isAdmin={role === 'admin'} canAct={canAct}
        onDecide={(id, verb, note) => run(
          `/api/v1/sre/autonomy/proposals/${id}/${verb}${q}`, { note })} />
      {families.map(f => (
        <FamilyTable key={f.family} family={f} canAct={canAct}
          onDowngrade={body => run(`/api/v1/sre/autonomy/downgrade${q}`, body)} />
      ))}
      <History items={history.data?.items || []} />
      <GameDays data={gameDays.data} />
      <Rollouts data={rollouts.data} />
    </section>
  )
}

function Header({ data }) {
  const bench = data?.view?.bench
  return (
    <div>
      <h2 className="text-sm font-semibold" style={strong}>Earned autonomy</h2>
      <p className="text-sm" style={muted}>
        Per incident family and action class. pg_sage proposes a promotion only from
        benchmark, shadow-review and live-recovery evidence; an admin approves it.
        Irreversible actions never go above L1.
      </p>
      <div className="mt-2 flex flex-wrap gap-3 text-xs" style={muted}>
        {data?.enforced
          ? <span className="rounded px-2 py-0.5"
            style={{ border: '1px solid var(--green)', color: 'var(--green)' }}>
            Enforced</span>
          : <span className="rounded px-2 py-0.5"
            style={{ border: '1px solid var(--red)', color: 'var(--red)' }}>
            Earned autonomy is not enforced (sre.autonomy.enforce: false)</span>}
        <span>Database: {data?.database}</span>
        <span>Bench report: {bench ? bench.generated_at : 'none ingested'}</span>
      </div>
      <FastElevation info={data?.fast_elevation} />
    </div>
  )
}

// FastElevation makes lowered trust-elevation settings visible: pg_sage
// can earn autonomy in hours rather than weeks under them.
function FastElevation({ info }) {
  if (!info?.active) return null
  return (
    <div data-testid="fast-elevation" className="mt-2 rounded px-2 py-1 text-xs"
      style={{ border: '1px solid var(--yellow)', color: 'var(--text-primary)' }}>
      <span className="font-semibold">Fast elevation</span>
      <span style={muted}> — these settings are below the spec, so trust is earned in
        hours. Irreversible actions, L4 and admin approval are unchanged.</span>
      <ul className="mt-1 list-disc pl-5">
        {(info.lowered || []).map(l => (
          <li key={l.key}>
            <code>{l.key}</code>: {l.value} {l.unit} (spec {l.default})
          </li>
        ))}
      </ul>
    </div>
  )
}

function PendingProposals({ families, isAdmin, canAct, onDecide }) {
  const [note, setNote] = useState('')
  const pending = families.flatMap(f => (f.classes || [])
    .filter(c => c.pending).map(c => c.pending))
  if (pending.length === 0) return null
  return (
    <div className="rounded border p-3 space-y-2" style={card}>
      <h3 className="text-sm font-semibold" style={strong}>Pending promotions</h3>
      {isAdmin && (
        <label className="block text-xs" style={muted}>
          Approval note
          <input className="ml-2 rounded px-2 py-1 text-sm" value={note}
            onChange={e => setNote(e.target.value)} />
        </label>
      )}
      {pending.map(p => (
        <div key={p.id} className="flex flex-wrap items-center gap-3 text-sm">
          <span style={strong}>{p.family} / {p.class}: {p.from} to {p.to}</span>
          <span className="text-xs" style={muted}>expires {p.expires_at}</span>
          {isAdmin && (
            <button className="rounded px-2 py-1 text-xs"
              style={{ border: '1px solid var(--green)' }}
              onClick={() => onDecide(p.id, 'approve', note)}>Approve</button>
          )}
          {canAct && (
            <button className="rounded px-2 py-1 text-xs"
              style={{ border: '1px solid var(--border)' }}
              onClick={() => onDecide(p.id, 'reject', note)}>Reject</button>
          )}
        </div>
      ))}
    </div>
  )
}

function FamilyTable({ family, canAct, onDowngrade }) {
  const sh = family.shadow || {}
  return (
    <div className="rounded border p-3" style={card}>
      <h3 className="text-sm font-semibold" style={strong}>{family.family}</h3>
      <div className="text-xs mb-2" style={muted}>
        Shadow: {sh.accepted ?? 0}/{sh.reviewed ?? 0} packets accepted in the window
      </div>
      <table className="w-full text-sm">
        <thead>
          <tr className="text-left text-xs" style={muted}>
            <th>Class</th><th>Reversibility</th><th>Cap</th><th>Granted</th>
            <th>Evidence</th><th>Effective</th><th>Next level</th><th />
          </tr>
        </thead>
        <tbody>
          {(family.classes || []).map(row => (
            <ClassRow key={row.class} family={family.family} row={row} canAct={canAct}
              onDowngrade={onDowngrade} />
          ))}
        </tbody>
      </table>
    </div>
  )
}

function ClassRow({ family, row, canAct, onDowngrade }) {
  return (
    <tr data-testid={`autonomy-row-${family}-${row.class}`} style={strong}>
      <td>
        <span>{row.class}</span>
        {row.provenance === 'carried_over' && (
          <div className="text-xs" style={muted}>
            carried over: {row.carried_ref}
          </div>
        )}
      </td>
      <td style={muted}>{row.reversibility}</td>
      <td data-testid="cap">{row.cap}</td>
      <td data-testid="granted">{row.granted}</td>
      <td data-testid="supported">{row.supported}</td>
      <td>
        <span data-testid="effective">{row.effective || row.granted}</span>
        {(row.downgrades || []).map(d => (
          <div key={d.reason} className="text-xs" style={{ color: 'var(--red)' }}>
            {d.reason}{d.detail ? `: ${d.detail}` : ''}
          </div>
        ))}
      </td>
      <td><NextChecks next={row.next} /></td>
      <td>{canAct && <DowngradeForm family={family} row={row} onSubmit={onDowngrade} />}</td>
    </tr>
  )
}

function NextChecks({ next }) {
  if (!next) return <span className="text-xs" style={muted}>at its cap</span>
  if (next.met) {
    return <span className="text-xs" style={muted}>{next.target}: evidence met</span>
  }
  const unmet = (next.checks || []).filter(c => !c.met)
  return (
    <ul className="text-xs" style={muted}>
      {unmet.map(c => (
        <li key={c.name}>{c.name}: {c.observed} (needs {c.required})</li>
      ))}
    </ul>
  )
}

function DowngradeForm({ family, row, onSubmit }) {
  const lower = LEVELS.slice(0, Math.max(LEVELS.indexOf(row.granted), 0))
  const [level, setLevel] = useState(lower[lower.length - 1] || '')
  const [reason, setReason] = useState('')
  if (lower.length === 0) return null
  const submit = () => {
    if (!reason.trim() || !level) return
    onSubmit({ family, class: row.class, level, reason: reason.trim() })
  }
  return (
    <div className="flex flex-wrap items-center gap-1 text-xs">
      <label>
        Downgrade to
        <select className="ml-1" value={level} onChange={e => setLevel(e.target.value)}>
          {lower.map(l => <option key={l} value={l}>{l}</option>)}
        </select>
      </label>
      <label>
        Reason
        <input className="ml-1 rounded px-1" value={reason}
          onChange={e => setReason(e.target.value)} />
      </label>
      <button className="rounded px-2 py-0.5"
        style={{ border: '1px solid var(--border)' }} onClick={submit}>Downgrade</button>
    </div>
  )
}

function History({ items }) {
  return (
    <div className="rounded border p-3" style={card}>
      <h3 className="text-sm font-semibold" style={strong}>History</h3>
      {items.length === 0 && <div className="text-xs" style={muted}>No changes yet.</div>}
      <ul className="text-xs space-y-1" style={muted}>
        {items.map(e => (
          <li key={e.id}>
            <span>{e.at}</span>{' '}
            <span style={strong}>{e.type}</span>{' '}
            <span>{e.family}/{e.class}</span>{' '}
            {e.from && e.to && <span>{e.from} to {e.to}</span>}{' '}
            <span>by {e.actor}</span>{e.database ? <span> on {e.database}</span> : null}
            <span>: {e.reason}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}

function GameDays({ data }) {
  const items = data?.items || []
  return (
    <div className="rounded border p-3 text-xs" style={card}>
      <h3 className="text-sm font-semibold" style={strong}>Game days</h3>
      {!data?.enabled && <div style={muted}>Off: needs sre.autonomy.game_days and a
        clone provider.</div>}
      <ul style={muted}>
        {items.map(g => (
          <li key={g.id}>{g.started_at} {g.status}{g.error ? `: ${g.error}` : ''}</li>
        ))}
      </ul>
    </div>
  )
}

function Rollouts({ data }) {
  const items = data?.items || []
  return (
    <div className="rounded border p-3 text-xs" style={card}>
      <h3 className="text-sm font-semibold" style={strong}>Fleet canaries</h3>
      {items.length === 0 && <div style={muted}>No canary rollouts.</div>}
      <ul style={muted}>
        {items.map(r => (
          <li key={r.id}>{r.created_at} {r.source_database}: {r.state}
            {r.halt_reason ? ` (${r.halt_reason})` : ''}, {r.applied_instances} applied</li>
        ))}
      </ul>
    </div>
  )
}
