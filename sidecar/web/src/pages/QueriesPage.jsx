import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { QueryState } from '../components/QueryState'
import { StackedAreaChart } from '../components/StackedAreaChart'

// QueriesPage: the workload explorer (market-01). The top table ranks the
// window's statements from the query store by load (execution seconds per
// wall-clock second), and the chart stacks the active sessions by wait
// class the collector samples each cycle. Each row opens the statement's
// query page.

const ORDERS = [
  { id: 'load', label: 'Load' }, { id: 'calls', label: 'Calls' },
  { id: 'mean', label: 'Mean time' }, { id: 'rows', label: 'Rows' },
]
const number = new Intl.NumberFormat('en-US', { maximumFractionDigits: 1 })
const PROVIDER_LABELS = {
  performance_insights: 'Performance Insights', lakebase: 'Lakebase wait counters',
}

const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }

function queryHref(row) {
  const params = new URLSearchParams({ queryid: row.queryid, database: row.database })
  return `#/query?${params}`
}

function TopRow({ row, fleet }) {
  return (
    <tr data-testid="top-query-row" className="border-t"
      style={{ borderColor: 'var(--border)' }}>
      {fleet && <td className="py-1 pr-3">{row.database}</td>}
      <td className="py-1 pr-3 font-mono">
        <a href={queryHref(row)} className="underline" style={{ color: 'var(--accent)' }}>
          {row.queryid}
        </a>
      </td>
      <td className="py-1 pr-3 font-mono text-xs break-all max-w-md">
        {row.query || <span style={{ color: 'var(--text-secondary)' }}>
          {row.text_unavailable || 'no statement text'}</span>}
      </td>
      <td className="py-1 pr-3 text-right">{row.load.toFixed(3)}</td>
      <td className="py-1 pr-3 text-right">{number.format(row.calls)}</td>
      <td className="py-1 pr-3 text-right">{number.format(row.mean_time_ms)} ms</td>
      <td className="py-1 text-right">{number.format(row.rows)}</td>
    </tr>
  )
}

function TopTable({ data, fleet }) {
  if (!data.queries?.length) {
    const why = data.coverage === 'not_measurable'
      ? 'No query has two samples in one statistics epoch in this window yet.'
      : 'No query samples in this window yet.'
    return <p data-testid="top-queries-empty" className="text-sm"
      style={{ color: 'var(--text-secondary)' }}>{why}</p>
  }
  return (
    <table data-testid="top-queries" className="w-full text-sm">
      <thead style={{ color: 'var(--text-secondary)' }}>
        <tr className="text-left">
          {fleet && <th className="pr-3">Database</th>}
          <th className="pr-3">Query ID</th><th className="pr-3">Statement</th>
          <th className="pr-3 text-right" title="Execution seconds per second">Load</th>
          <th className="pr-3 text-right">Calls</th>
          <th className="pr-3 text-right">Mean</th><th className="text-right">Rows</th>
        </tr>
      </thead>
      <tbody>
        {data.queries.map(row => (
          <TopRow key={`${row.database}:${row.queryid}`} row={row} fleet={fleet} />
        ))}
      </tbody>
    </table>
  )
}

function ProviderWaits({ provider }) {
  const label = PROVIDER_LABELS[provider.source] || provider.source
  const entries = Object.entries(provider.load_by_wait || {}).sort((a, b) => b[1] - a[1])
  return (
    <p data-testid="wait-provider" className="text-xs mb-2"
      style={{ color: 'var(--text-secondary)' }}>
      Preferred source: {label} (current load by wait class:{' '}
      {entries.map(([cls, v]) => `${cls} ${v.toFixed(2)}`).join(', ')}).
      {provider.unit_confirmed === false &&
        ` Wait time unit not yet confirmed (assumed ${provider.wait_event_time_unit}).`}
    </p>
  )
}

function WaitLoad({ database }) {
  const pick = !database || database === 'all'
  const wait = useAPI(pick ? null
    : `/api/v1/wait-load?database=${encodeURIComponent(database)}`, 60000)
  if (pick) {
    return <p data-testid="wait-load-pick" className="text-sm"
      style={{ color: 'var(--text-secondary)' }}>
      Select a database to see its wait-class load.</p>
  }
  return (
    <QueryState query={wait}>
      {data => (
        <div data-testid="wait-load">
          <p className="text-xs mb-2" style={{ color: 'var(--text-secondary)' }}>
            Active sessions by wait class, {data.sampling || 'sampled each collector cycle'}
            {' '}from {data.source}.
          </p>
          {data.provider && <ProviderWaits provider={data.provider} />}
          <StackedAreaChart series={data.series || []} classes={data.classes || []} />
        </div>
      )}
    </QueryState>
  )
}

export function QueriesPage({ database }) {
  const [order, setOrder] = useState('load')
  const scope = database || 'all'
  const top = useAPI(`/api/v1/queries/top?database=${encodeURIComponent(scope)}`
    + `&order=${order}`, 60000)
  const fleet = scope === 'all'
  return (
    <div className="space-y-6">
      <section className="rounded p-4" style={card}>
        <h2 className="text-sm font-semibold mb-3" style={{ color: 'var(--text-primary)' }}>
          Wait-class load
        </h2>
        <WaitLoad database={database} />
      </section>
      <section className="rounded p-4" style={card}>
        <div className="flex flex-wrap items-center gap-2 mb-3">
          <h2 className="text-sm font-semibold flex-1" style={{ color: 'var(--text-primary)' }}>
            Top statements, last hour
          </h2>
          {ORDERS.map(o => (
            <button key={o.id} type="button" onClick={() => setOrder(o.id)}
              aria-pressed={order === o.id} className="px-2 py-1 rounded text-xs"
              style={{ border: '1px solid var(--border)',
                color: order === o.id ? 'var(--accent)' : 'var(--text-secondary)' }}>
              {o.label}
            </button>
          ))}
        </div>
        <QueryState query={top}>
          {data => <TopTable data={data} fleet={fleet} />}
        </QueryState>
      </section>
    </div>
  )
}
