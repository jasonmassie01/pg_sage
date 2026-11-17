import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { QueryState } from '../../components/QueryState'
import { POLICY_GATE_DOC } from '../settings/StandingPolicy'
import { reasonText, riskLabel, verdictLabel } from './workflow'
import { formatTime } from '../../lib/time'

// Actions > Held back (policy-07; the route key stays "decisions"): why a
// change has not run. Each row is
// one candidate the standing gate withheld (queued, parked, blocked or
// observe-only) and has not run since, from GET /api/v1/decisions: the
// verdict and the reason in words with a next step (gateLabels.js, ux-05b),
// how often the gate repeated it and, for a budget park, when the budget
// frees. A row links to its exact approval card or finding. Operators
// only: the route refuses viewers.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
// The filter's verdicts, in the ledger's spelling, named as the rows name them.
const VERDICTS = [['', 'Every verdict'], ...['queue_approval', 'parked', 'blocked',
  'observe_only'].map(value => [value, verdictLabel(value)])]

function decisionsURL(database, verdict) {
  const params = new URLSearchParams()
  if (database && database !== 'all') params.set('database', database)
  if (verdict) params.set('verdict', verdict)
  const query = params.toString()
  return `/api/v1/decisions${query ? `?${query}` : ''}`
}

export function DecisionsTab({ database }) {
  const [verdict, setVerdict] = useState('')
  const query = useAPI(decisionsURL(database, verdict))
  return (
    <section className="space-y-3" data-testid="decisions-tab">
      <p className="text-sm" style={muted}>
        Changes the standing policy has withheld and not run since, newest first. The
        reasons are listed in the <a className="underline" style={{ color: 'var(--accent)' }}
          href={POLICY_GATE_DOC}>
          policy gate reference</a>.
      </p>
      <label className="text-sm flex items-center gap-2" style={muted}>
        Verdict
        <select data-testid="decisions-verdict" value={verdict}
          onChange={e => setVerdict(e.target.value)} className="rounded px-2 py-1"
          style={{ background: 'var(--bg-card)', border: '1px solid var(--border)', ...strong }}>
          {VERDICTS.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
        </select>
      </label>
      <QueryState query={query}>
        {data => <DecisionsList data={data} database={database} />}
      </QueryState>
    </section>
  )
}

function DecisionsList({ data, database }) {
  const decisions = data?.decisions || []
  const partial = data?.partial || []
  return <>
    {partial.length > 0 && <p role="status" data-testid="decisions-partial" className="text-sm"
      style={{ color: 'var(--yellow)' }}>
      Decisions could not be read from {partial.map(p => `${p.database} (${p.error})`)
        .join(', ')}; the list shows the other databases.
    </p>}
    {decisions.length === 0
      ? <p data-testid="decisions-empty" className="text-sm" style={muted}>
        Nothing is withheld: every change the gate decided has run or been handed to a
        person.</p>
      : <ul className="space-y-2">
        {decisions.map(d => <DecisionRow key={`${d.database}:${d.decision_id}`} decision={d}
          database={database} />)}
      </ul>}
    {data?.truncated && <p data-testid="decisions-truncated" className="text-xs" style={muted}>
      Showing the newest {data.limit}; narrow the verdict or the database to see the rest.
    </p>}
  </>
}

// decisionLink focuses the row's approval request on Needs your review
// (tab= and queue=, ux-01b) or its finding on Cases (finding=, caseFocus.js).
// queue= is a queue id, never action= (an executed change record's id).
function decisionLink(d) {
  const link = (path, params) => `#/${path}?${new URLSearchParams(params)}`
  const db = d.database ? { database: d.database } : {}
  if (d.queue_id) {
    return { href: link('actions', { tab: 'pending', queue: d.queue_id, ...db }),
      label: `Approval request #${d.queue_id}` }
  }
  if (d.finding_id) {
    return { href: link('cases', { finding: d.finding_id, ...db }),
      label: `Finding #${d.finding_id}` }
  }
  return null
}

function DecisionRow({ decision: d, database }) {
  const link = decisionLink(d)
  const targets = (d.target_objects || []).join(', ')
  return (
    <li data-testid="decision-row" className="rounded border p-3 text-sm"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <div className="flex flex-wrap items-baseline gap-2">
        <span className="font-medium text-xs" style={strong}>{verdictLabel(d.verdict)}</span>
        <span style={strong}>{reasonText(d.reason)}</span>
      </div>
      <p className="mt-1 text-xs" style={muted}>
        {(!database || database === 'all') && <span>{d.database} · </span>}
        {d.feature}{targets && ` on ${targets}`} · Risk: {riskLabel(d.risk_tier)} · seen
        {' '}{d.repeat_count}{' '}{d.repeat_count === 1 ? 'time' : 'times'}, last
        {' '}{decidedAt(d.last_seen_at)}
        {d.policy_version ? ` · policy v${d.policy_version}` : ''}
      </p>
      {d.budget_detail && <p data-testid="decision-budget" className="mt-1 text-xs"
        style={muted}>Budget: {d.budget_detail}</p>}
      {d.verification_wait_detail && <p className="mt-1 text-xs" style={muted}>
        Waiting on: {d.verification_wait_detail}</p>}
      {d.deadline_kind && <p className="mt-1 text-xs" style={muted}>
        Deadline: {d.deadline_kind} by {decidedAt(d.deadline_hard_at)}</p>}
      {link && <a className="mt-1 inline-block text-xs underline" href={link.href}
        style={{ color: 'var(--accent)' }}>{link.label}</a>}
    </li>
  )
}

// decidedAt names a held-back decision's time; missing or unreadable is
// "unknown", as before ux-05d.
function decidedAt(value) {
  if (!value) return 'unknown'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? 'unknown' : formatTime(date)
}
