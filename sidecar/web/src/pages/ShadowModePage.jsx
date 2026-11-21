import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { QueryState } from '../components/QueryState'
import { queryBlocked } from '../lib/queryState'
import { ShadowDecisions } from './trust/ShadowDecisions'

// Shadow Mode reads the shadow ledger (GET /api/v1/shadow-decisions): below
// a class's earned trust level pg_sage records every action it would have
// taken, runs none of them, and scores each later from what happened. Per
// class: the decisions, how they scored, how many count as shadow evidence,
// and the toil minutes the counted correct ones would have saved
// (potential, never credited; the Value page prices verified outcomes).

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }

function dbParam(database) {
  return database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}`
    : ''
}

function potential(minutes) {
  return Number.isFinite(minutes) ? `${minutes} min potential` : 'not modelled'
}

const COLUMNS = [
  ['total', 'Decisions'], ['pending', 'Pending'], ['correct', 'Correct'],
  ['incorrect', 'Incorrect'], ['counted', 'Counted'],
]

function ClassRow({ database, row }) {
  return (
    <tr data-testid={`shadow-class-${database}-${row.class}`}
      style={{ borderTop: '1px solid var(--border)' }}>
      <td className="py-1 pr-3 align-top">
        <div style={strong}>{row.class}</div>
        <div className="text-xs" style={muted}>{row.family}</div>
        <ShadowDecisions database={database} cls={row.class} />
      </td>
      {COLUMNS.map(([key]) => (
        <td key={key} className="py-1 pr-3 align-top" data-testid={`shadow-${key}`}>
          {row[key] || 0}
        </td>
      ))}
      <td className="py-1 align-top" data-testid="shadow-potential">
        {potential(row.potential_minutes)}
      </td>
    </tr>
  )
}

function DatabaseLedger({ db }) {
  return (
    <div className="rounded border p-3 text-sm overflow-x-auto" style={card}>
      <div className="font-medium mb-1" style={strong}>{db.database}</div>
      <table className="w-full text-left">
        <thead>
          <tr className="text-xs" style={muted}>
            <th scope="col" className="pr-3 font-normal">Class</th>
            {COLUMNS.map(([key, label]) => (
              <th scope="col" key={key} className="pr-3 font-normal">{label}</th>
            ))}
            <th scope="col" className="font-normal">Potential minutes</th>
          </tr>
        </thead>
        <tbody>
          {db.summary.map(row => (
            <ClassRow key={row.class} database={db.database} row={row} />
          ))}
        </tbody>
      </table>
    </div>
  )
}

// levelOf is a trust row's effective level (the one the gate holds it
// to), else its stored one. The API serves labels ("L3"); an unreadable
// level is -1, so it never counts as L3.
function levelOf(row) {
  const m = /^L?([0-4])$/i.exec(String(row.effective ?? row.level ?? ''))
  return m ? Number(m[1]) : -1
}

// emptyReason says why nothing was shadowed, from the trust ledger: it is
// not enforced, every self-initiated class is already at L3 (nothing is
// below its earned level), or nothing has been recorded yet.
function emptyReason(trust) {
  if (!trust.data && trust.loading) {
    return 'Reading the trust ledger for the reason.'
  }
  if (!trust.data) {
    return `The trust ledger could not be read${trust.error ? ` (${trust.error})` : ''}; `
      + 'pg_sage shadows only classes below their earned level in that ledger.'
  }
  if (trust.data.enforced === false) {
    return 'sre.autonomy.enforce is off: the trust ledger grants no levels, so no class '
      + 'is below one and nothing is shadowed.'
  }
  const rows = (trust.data.databases || []).flatMap(db => db.rows || [])
    .filter(row => row.kind === 'self_initiated')
  if (rows.length > 0 && rows.every(row => levelOf(row) >= 3)) {
    return 'Every class is already at L3 (unattended), so none is below its earned '
      + 'level and nothing is shadowed.'
  }
  return 'Nothing recorded yet: pg_sage records a shadow decision when it withholds an '
    + 'action below a class\'s earned level.'
}

function EmptyLedger({ trust }) {
  return (
    <div data-testid="shadow-empty" className="rounded border p-3 text-sm" style={card}>
      <div style={strong}>No shadow decisions yet.</div>
      <div style={muted}>{emptyReason(trust)}</div>
    </div>
  )
}

export function ShadowModePage({ database }) {
  const ledger = useAPI(`/api/v1/shadow-decisions${dbParam(database)}`, 30000)
  const trust = useAPI(`/api/v1/trust${dbParam(database)}`, 30000)

  if (queryBlocked(ledger)) {
    return <QueryState query={ledger} skeleton={<LoadingSpinner />} />
  }
  const databases = (ledger.data?.databases || [])
    .map(db => ({ ...db, summary: db.summary || [] }))
    .filter(db => db.summary.length > 0)
  return (
    <QueryState query={ledger}><section className="space-y-4" data-testid="shadow-mode-report">
      <div>
        <h2 className="text-sm font-semibold" style={strong}>Shadow Mode</h2>
        <p className="text-sm" style={muted}>
          What pg_sage would have done below each class&apos;s earned autonomy, and how
          it scored. Nothing here ran. The potential column prices the counted correct
          decisions with the toil model; only verified outcomes count on the Value page.
        </p>
      </div>
      {databases.length === 0
        ? <EmptyLedger trust={trust} />
        : databases.map(db => <DatabaseLedger key={db.database} db={db} />)}
    </section></QueryState>
  )
}
