import { useState } from 'react'
import { QueryState } from '../../components/QueryState'
import { formatTime } from '../../lib/time'
import { day, jsonText } from './policySetsFormat'

// PolicyExceptions is the Exceptions list (fleetpol-01c): every database
// whose own document is in force, how it differs from its set field by
// field, who set it, why and until when. An exception without an expiry
// is flagged and counted: it holds until someone replaces it.

const muted = { color: 'var(--text-secondary)' }
const cell = 'py-1 pr-3 align-top'

function Expiry({ value, now }) {
  if (!value) {
    return <span data-testid="policy-exception-no-expiry" style={{ color: 'var(--yellow)' }}>
      No expiry</span>
  }
  const at = new Date(value)
  const label = formatTime(value)
  if (at.getTime() <= now) {
    return <span data-testid="policy-exception-expired" style={{ color: 'var(--yellow)' }}>
      {label}: expired, the set's document applies again within a minute</span>
  }
  if (day(at) === day(now)) {
    return <span>{label} <span data-testid="policy-exception-expires-today"
      style={{ color: 'var(--yellow)' }}>(expires today)</span></span>
  }
  return <span>{label}</span>
}

function Diff({ diff }) {
  if (!diff || diff.length === 0) {
    return <span data-testid="policy-exception-same" style={muted}>No difference from the set</span>
  }
  return (
    <ul data-testid="policy-exception-diff" className="space-y-0.5">
      {diff.map(d => <li key={d.field}>
        <code>{d.field}</code>: set {jsonText(d.set)}, this database {jsonText(d.exception)}
      </li>)}
    </ul>
  )
}

function ExceptionRow({ exc, names, now }) {
  return (
    <tr data-testid="policy-exception" style={{ borderTop: '1px solid var(--border)' }}>
      <td className={cell}>{names[exc.database_id] || `database #${exc.database_id}`}</td>
      <td className={cell}>{exc.set_name}</td>
      <td className={cell}><Diff diff={exc.diff} /></td>
      <td className={cell}>{exc.set_by}</td>
      <td className={cell}>{exc.reason}</td>
      <td className={cell}><Expiry value={exc.expires_at} now={now} /></td>
    </tr>
  )
}

function ExceptionTable({ list, names, now }) {
  if (list.length === 0) {
    return <p data-testid="policy-exceptions-empty" style={muted}>
      No exceptions: every database follows its set&apos;s document.</p>
  }
  const unbounded = list.filter(e => !e.expires_at).length
  return <>
    <p className="text-xs" style={muted}>Without an expiry:{' '}
      <span data-testid="policy-exceptions-unbounded">{unbounded}</span></p>
    <div className="overflow-x-auto">
      <table className="w-full text-xs">
        <thead style={muted}><tr>
          {['Database', 'Set', 'Differs from the set', 'Set by', 'Why', 'Expires']
            .map(h => <th key={h} className={`${cell} text-left font-medium`}>{h}</th>)}
        </tr></thead>
        <tbody>{list.map(e => <ExceptionRow key={e.database_id} exc={e} names={names}
          now={now} />)}</tbody>
      </table>
    </div>
  </>
}

// now is the time of the last successful read, so expiry is judged
// against the data on screen.
export function PolicyExceptions({ query, names }) {
  const [mountedAt] = useState(() => Date.now())
  const now = query.updatedAt || mountedAt
  return (
    <section className="space-y-2" data-testid="policy-exceptions">
      <h4 className="font-medium" style={{ color: 'var(--text-primary)' }}>Exceptions</h4>
      <QueryState query={query}>
        {data => <ExceptionTable list={data?.exceptions || []} names={names}
          now={now} />}
      </QueryState>
    </section>
  )
}
