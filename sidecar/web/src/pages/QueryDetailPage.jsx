import { useAPI } from '../hooks/useAPI'
import { QueryState } from '../components/QueryState'
import { SQLBlock } from '../components/SQLBlock'
import { QueryCallSite } from './QueryCallSite'
import { queryDetailParams } from '../lib/workload'
import { formatTime } from '../lib/time'

// QueryDetailPage: one statement of the workload explorer (market-01):
// its history from the query store, one marker per plan change, the
// newest captured plan, the cases, hints and actions about it, and the
// application call site when a code host is configured (pr-01a). A
// queryid belongs to one database, so the page needs one.

const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
const muted = { color: 'var(--text-secondary)' }
const number = new Intl.NumberFormat('en-US', { maximumFractionDigits: 1 })


function Section({ title, testId, children }) {
  return (
    <section data-testid={testId} className="rounded p-4" style={card}>
      <h2 className="text-sm font-semibold mb-2" style={{ color: 'var(--text-primary)' }}>
        {title}
      </h2>
      {children}
    </section>
  )
}

function History({ data }) {
  const changes = new Map((data.plan_changes || []).map(c => [c.at, c]))
  if (!data.buckets?.length) {
    return <p className="text-sm" style={muted}>
      {data.coverage === 'none' ? 'No samples of this query in the window.'
        : 'Not measurable: the window has fewer than two samples in one epoch.'}</p>
  }
  return (
    <table className="w-full text-xs">
      <thead style={muted}>
        <tr className="text-left"><th>Bucket</th><th className="text-right">Calls</th>
          <th className="text-right">Mean</th><th className="text-right">Rows</th><th /></tr>
      </thead>
      <tbody>
        {data.buckets.map(b => (
          <tr key={b.start} className="border-t" style={{ borderColor: 'var(--border)' }}>
            <td>{formatTime(b.start)}</td>
            <td className="text-right">{number.format(b.calls)}</td>
            <td className="text-right">{number.format(b.mean_time_ms)} ms</td>
            <td className="text-right">{number.format(b.rows)}</td>
            <td className="pl-2">{b.epoch_break ? 'statistics reset' : ''}</td>
          </tr>
        ))}
      </tbody>
      {changes.size > 0 && (
        <tfoot>
          {[...changes.values()].map(c => (
            <tr key={c.at} data-testid="plan-change">
              <td colSpan={5} className="pt-1" style={{ color: 'var(--yellow)' }}>
                Plan changed at {formatTime(c.at)}: {c.from} → {c.to}
              </td>
            </tr>
          ))}
        </tfoot>
      )}
    </table>
  )
}

function Plan({ plan }) {
  if (!plan) return <p className="text-sm" style={muted}>No plan captured yet.</p>
  return (
    <div className="text-xs space-y-1">
      <p>{plan.node_types?.join(' → ') || 'no plan nodes'}</p>
      <p style={muted}>Source {plan.source}, cost {plan.total_cost}
        {plan.plan_hash ? `, fingerprint ${plan.plan_hash}` : ''}</p>
      {plan.plan_json && (
        <details><summary className="cursor-pointer">Raw plan</summary>
          <pre className="mt-1 whitespace-pre-wrap break-all">
            {JSON.stringify(plan.plan_json, null, 2)}</pre>
        </details>
      )}
    </div>
  )
}

function Links({ data, database }) {
  const db = encodeURIComponent(database)
  return (
    <div className="text-xs space-y-2">
      <ul>
        {(data.cases || []).map(c => (
          <li key={c.finding_id}>
            <a className="underline" style={{ color: 'var(--accent)' }}
              href={`#/cases?finding=${c.finding_id}&database=${db}`}>{c.title}</a>
            {' '}<span style={muted}>{c.severity}, {c.status}</span>
          </li>
        ))}
      </ul>
      <ul data-testid="query-hints">
        {(data.hints || []).map(h => <li key={h.id}>Hint {h.hint_text} ({h.status})</li>)}
      </ul>
      <ul>
        {(data.actions || []).map(a => (
          <li key={a.id}>Action #{a.id} {a.action_type}: {a.outcome}</li>
        ))}
      </ul>
      {!data.cases?.length && !data.hints?.length && !data.actions?.length && (
        <p style={muted}>No cases, hints or actions about this query.</p>
      )}
    </div>
  )
}

export function QueryDetailPage({ route, database }) {
  const { queryid, database: db } = queryDetailParams(route, database)
  const ok = Boolean(queryid && db)
  const detail = useAPI(ok
    ? `/api/v1/queries/${queryid}?database=${encodeURIComponent(db)}` : null, 60000)
  if (!db) {
    return <p data-testid="query-detail-pick" className="text-sm" style={muted}>
      Select a database: a query ID belongs to one database.</p>
  }
  if (!queryid) return <p className="text-sm" style={muted}>The link has no valid query ID.</p>
  return (
    <QueryState query={detail}>
      {data => (
        <div className="space-y-4">
          <Section title={`Query ${data.queryid} on ${data.database}`}>
            {data.query ? <SQLBlock sql={data.query} />
              : <p className="text-sm" style={muted}>{data.text_unavailable}</p>}
            <QueryCallSite queryid={queryid} database={db} />
          </Section>
          <Section title="History, last 24 hours" testId="query-history">
            <History data={data} />
          </Section>
          <Section title="Latest captured plan" testId="query-plan">
            <Plan plan={data.plan} />
          </Section>
          <Section title="Cases, hints and actions"><Links data={data} database={db} /></Section>
        </div>
      )}
    </QueryState>
  )
}
