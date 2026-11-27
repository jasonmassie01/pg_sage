import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { QueryState } from '../components/QueryState'
import { LiveTimeAgo } from '../components/TimeAgo'
import { ROLLBACK_LABELS as ROLLBACK, VERDICT_LABELS as VERDICTS } from '../lib/outcomes'
import { ledgerTierLabel } from './actions/LadderSection'
import { ValuePage } from './ValuePage'
import { ExternalAgentRequests } from './mcptokens/ExternalAgentRequests'
import { routeParam } from '../routes'

// LedgerPage (ux-02a): what pg_sage changed in a window, one row per
// change from GET /api/v1/outcomes. The API computes every number (effect
// percent, the measured share); this page only words and formats them.
// #/ledger?evidence=<id> shows the changes that evidence id cites, over
// the whole retention. ux-11: Value is the ledger's summary (#/value
// redirects to section=value) and every external agent request is kept here
// (section=agents); each section mounts, and reads, only once open.

const pollMs = 60000
const RETENTION = '9600h'
const WINDOWS = [['24h', 'Last 24 hours'], ['168h', 'Last 7 days'], ['720h', 'Last 30 days']]

const ORIGINS = {
  plan: 'Plan step', approved: 'Approved by a person', agent: 'Agent under its grant',
  autonomous: 'Autonomous under policy',
}


function ledgerURL(since, database, evidence) {
  const params = new URLSearchParams({ since: evidence ? RETENTION : since })
  if (database && database !== 'all') params.set('database', database)
  if (evidence) params.set('evidence', evidence)
  return `/api/v1/outcomes?${params.toString()}`
}

function pct(effect) {
  const v = Number(effect.change_pct)
  return `${v > 0 ? '+' : ''}${v}%`
}

function Measured({ row }) {
  if (!row.measured) return <span>Not measured: {row.measured_reason || 'no measurement'}</span>
  return (
    <span>
      {pct(row.measured)} {row.measured.metric}
      {row.expected && (
        <span className="ml-1 text-xs" style={{ color: 'var(--text-secondary)' }}>
          (expected {pct(row.expected)})
        </span>
      )}
    </span>
  )
}

function Links({ row }) {
  const caseHref = row.finding_id
    ? `#/cases?finding=${encodeURIComponent(row.finding_id)}` +
      `&database=${encodeURIComponent(row.database)}`
    : ''
  return (
    <span className="flex flex-col gap-1">
      <a className="underline" href={`#/actions?action=${encodeURIComponent(row.action_id)}`}
        aria-label={`Open action ${row.action_id}`}>Action #{row.action_id}</a>
      {caseHref && <a className="underline" href={caseHref}
        aria-label={`Open issue for action ${row.action_id}`}>Issue</a>}
      {row.evidence_id && <span className="font-mono text-xs">{row.evidence_id}</span>}
    </span>
  )
}

function LedgerRow({ row }) {
  return (
    <tr data-testid={`ledger-row-${row.database}-${row.action_id}`}
      style={{ borderTop: '1px solid var(--border)' }}>
      <td className="p-2 whitespace-nowrap"><LiveTimeAgo timestamp={row.executed_at} /></td>
      <td className="p-2">{row.database}</td>
      <td className="p-2">
        <div className="font-medium">{row.what}</div>
        {row.why && <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          {row.why}</div>}
        <div className="text-xs" style={{ color: 'var(--text-secondary)' }}>
          {ORIGINS[row.origin] || row.origin}</div>
      </td>
      <td className="p-2">
        {VERDICTS[row.verdict] || row.verdict}
        {ledgerTierLabel(row.tier) && (
          <div data-testid={`ledger-tier-${row.database}-${row.action_id}`}
            className="text-xs" style={{ color: 'var(--text-secondary)' }}>
            {ledgerTierLabel(row.tier)}
          </div>
        )}
      </td>
      <td className="p-2"><Measured row={row} /></td>
      <td className="p-2">{ROLLBACK[row.rollback_state] || row.rollback_state}</td>
      <td className="p-2"><Links row={row} /></td>
    </tr>
  )
}

function Summary({ summary }) {
  const s = summary || {}
  const measured = s.measured_share === null || s.measured_share === undefined
    ? 'nothing to measure'
    : `${s.measured} of ${s.changes} with a measured effect`
  return (
    <p data-testid="ledger-summary" className="text-sm">
      {s.changes} changes · {s.verified} verified · {s.reverted} reverted ·{' '}
      {s.inconclusive} inconclusive · {s.pending} not judged yet · {measured}
    </p>
  )
}

function ReadErrors({ errors }) {
  if (!errors?.length) return null
  return (
    <div role="alert" className="rounded border p-3 text-sm"
      style={{ borderColor: 'var(--yellow)' }}>
      Some databases could not be read; their changes are missing, not absent:
      <ul className="mt-1 list-disc pl-5">
        {errors.map(e => <li key={e.database}>{e.error}</li>)}
      </ul>
    </div>
  )
}

function LedgerTable({ rows }) {
  if (!rows?.length) {
    return <p role="status" className="text-sm">No changes in this window.</p>
  }
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-sm">
        <thead>
          <tr className="text-left" style={{ color: 'var(--text-secondary)' }}>
            {['When', 'Database', 'Change', 'Verdict', 'Measured', 'Rollback', 'Links']
              .map(h => <th key={h} scope="col" className="p-2 font-medium">{h}</th>)}
          </tr>
        </thead>
        <tbody>{rows.map(r => <LedgerRow key={`${r.database}-${r.action_id}`} row={r} />)}
        </tbody>
      </table>
    </div>
  )
}

// Section is a collapsed part of the page that mounts its content only once
// open.
function Section({ testId, title, initiallyOpen, children }) {
  const [open, setOpen] = useState(initiallyOpen)
  return (
    <details data-testid={testId} open={open} className="rounded border p-3"
      style={{ borderColor: 'var(--border)' }}
      onToggle={e => setOpen(e.currentTarget.open)}>
      <summary className="cursor-pointer text-sm font-semibold">{title}</summary>
      {open && <div className="mt-3">{children}</div>}
    </details>
  )
}

export function LedgerPage({ database, user, route }) {
  const [since, setSince] = useState('24h')
  const hash = route ?? window.location.hash
  const evidence = routeParam(hash, 'evidence')
  const section = routeParam(hash, 'section')
  const canReview = user?.role === 'admin' || user?.role === 'operator'
  const ledger = useAPI(ledgerURL(since, database, evidence), pollMs)
  return (
    <div className="space-y-4" data-testid="ledger-page">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Outcome ledger</h1>
          <p className="mt-1 text-sm" style={{ color: 'var(--text-secondary)' }}>
            What pg_sage changed, what it expected, what it measured and whether it can
            still be rolled back.
          </p>
        </div>
        {evidence ? (
          <p data-testid="ledger-evidence-filter" className="text-sm">
            Changes citing evidence <span className="font-mono">{evidence}</span>{' '}
            <a className="underline" href="#/ledger">Show all</a>
          </p>
        ) : (
          <label className="text-sm">
            <span className="mr-2">Window</span>
            <select value={since} onChange={e => setSince(e.target.value)}
              className="rounded px-2 py-1" style={{ background: 'var(--bg-card)' }}>
              {WINDOWS.map(([v, l]) => <option key={v} value={v}>{l}</option>)}
            </select>
          </label>
        )}
      </div>
      <Section testId="ledger-value" title="Value: verified work and DBA hours saved"
        initiallyOpen={section === 'value'}>
        <ValuePage database={database} user={user} embedded />
      </Section>
      <QueryState query={ledger}>
        {/* An answer with no body is no ledger yet, never a crash. */}
        {data => (data ? (
          <>
            <Summary summary={data.summary} />
            <ReadErrors errors={data.errors} />
            <LedgerTable rows={data.rows} />
          </>
        ) : null)}
      </QueryState>
      {canReview && (
        <Section testId="ledger-agent-requests" title="External agent requests"
          initiallyOpen={section === 'agents'}>
          <ExternalAgentRequests />
        </Section>
      )}
    </div>
  )
}
