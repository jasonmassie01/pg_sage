import { useEffect, useState } from 'react'
import { apiSend } from '../lib/api'
import { exactQueryID, planSourceLabel } from '../lib/explain'

// tuning-10: the natural-language EXPLAIN of one pg_stat_statements query.
// The plan rules' entries are always there (no LLM needed); the model's,
// when an LLM is configured, are labelled apart. The query id travels as a
// decimal string: a JavaScript number is exact only to 2^53.

const SOURCE_LABELS = { rules: 'rules', llm: 'model' }

function explainURL(database) {
  if (database && database !== 'all') {
    return `/api/v1/explain?database=${encodeURIComponent(database)}`
  }
  return '/api/v1/explain'
}

// entriesOf pairs each list entry with its source; a result without
// insights (cached before tuning-10) shows its entries unlabelled.
function entriesOf(result, kind, list) {
  const tagged = (result.insights || []).filter(i => i.kind === kind)
  return (list || []).map(text => {
    const match = tagged.find(i => i.text === text)
    return { text, source: match ? match.source : '' }
  })
}

function SourceBadge({ source }) {
  if (!SOURCE_LABELS[source]) return null
  return (
    <span className="ml-2 px-1.5 py-0.5 rounded text-[10px] font-medium uppercase"
      style={{ border: '1px solid var(--border)', color: 'var(--text-secondary)' }}>
      {SOURCE_LABELS[source]}
    </span>
  )
}

function EntryList({ title, entries, empty, testID }) {
  return (
    <div className="mt-3" data-testid={testID}>
      <div className="font-medium mb-1" style={{ color: 'var(--text-primary)' }}>{title}</div>
      {entries.length === 0
        ? <p style={{ color: 'var(--text-secondary)' }}>{empty}</p>
        : (
          <ul className="list-disc pl-5 space-y-1">
            {entries.map(e => (
              <li key={`${e.source}:${e.text}`}>
                <span>{e.text}</span><SourceBadge source={e.source} />
              </li>
            ))}
          </ul>
        )}
    </div>
  )
}

function formatMs(ms) {
  return ms == null ? '--' : `${ms.toFixed(2)} ms`
}

function NodeTable({ nodes }) {
  if (!nodes || nodes.length === 0) return null
  return (
    <div className="mt-3 overflow-x-auto">
      <table className="w-full text-left" data-testid="explain-nodes">
        <thead style={{ color: 'var(--text-secondary)' }}>
          <tr><th>Node</th><th>Time (all loops)</th><th>Loops</th><th>Rows</th>
            <th>Estimate</th><th>Warning</th></tr>
        </thead>
        <tbody>
          {nodes.map((n, i) => (
            <tr key={`${i}-${n.node_type}`}>
              <td>{n.description}</td>
              <td>{formatMs(n.time_ms)}</td>
              <td>{n.loops ?? '--'}</td>
              <td>{n.time_ms == null ? '--' : n.rows}</td>
              <td>{n.row_estimate}</td>
              <td>{n.warning || ''}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function ExplainBody({ result }) {
  const source = planSourceLabel(result)
  return (
    <>
      <p style={{ color: 'var(--text-primary)' }}>{result.summary}</p>
      {source && (
        <p className="mt-1" data-testid="explain-plan-source"
          style={{ color: 'var(--text-secondary)' }}>Plan: {source}</p>
      )}
      {result.note && <p className="mt-1" style={{ color: 'var(--text-secondary)' }}>
        {result.note}</p>}
      <EntryList title="Why it is slow" testID="explain-slow-because" empty="No reasons found."
        entries={entriesOf(result, 'slow_because', result.slow_because)} />
      <EntryList title="Proposed changes" testID="explain-recommendations"
        empty="No proposed changes."
        entries={entriesOf(result, 'recommendation', result.recommendations)} />
      <NodeTable nodes={result.node_breakdown} />
    </>
  )
}

// ExplainPanel explains queryid (an exact decimal string) on database. A
// failed request shows the server's message with Retry, which asks again.
export function ExplainPanel({ database, queryid, onClose }) {
  const [state, setState] = useState({ loading: true })
  const [attempt, setAttempt] = useState(0)
  useEffect(() => {
    let live = true
    apiSend('POST', explainURL(database), { query_id: queryid })
      .then(result => { if (live) setState({ result }) })
      .catch(err => { if (live) setState({ error: err.message }) })
    return () => { live = false }
  }, [database, queryid, attempt])
  return (
    <section className="mt-2 rounded border p-3 text-xs" data-testid="explain-panel"
      aria-label={`Explain of query ${queryid}`} style={{ borderColor: 'var(--border)' }}>
      <div className="flex items-center justify-between">
        <span className="font-medium" style={{ color: 'var(--text-primary)' }}>
          Explain of query {queryid}
        </span>
        {onClose && (
          <button type="button" onClick={onClose} aria-label="Close explain"
            className="px-2 py-0.5 rounded" style={{ border: '1px solid var(--border)' }}>
            Close
          </button>
        )}
      </div>
      {state.loading && <p className="mt-2" style={{ color: 'var(--text-secondary)' }}>
        Explaining…</p>}
      {state.error && <div className="mt-2 flex items-center gap-2">
        <p role="alert" style={{ color: 'var(--red)' }}>{state.error}</p>
        <button type="button" onClick={() => {
          setState({ loading: true })
          setAttempt(n => n + 1)
        }}
          className="px-2 py-0.5 rounded" style={{ border: '1px solid var(--border)' }}>
          Retry
        </button>
      </div>}
      {state.result && <div className="mt-2"><ExplainBody result={state.result} /></div>}
    </section>
  )
}

// ExplainQueryButton opens the panel for an exact query id; without one
// it renders nothing.
export function ExplainQueryButton({ database, queryid }) {
  const [open, setOpen] = useState(false)
  const id = exactQueryID(queryid)
  if (!id) return null
  return (
    <div className="mt-2">
      {!open && (
        <button type="button" onClick={() => setOpen(true)}
          className="px-2 py-1 rounded text-xs" style={{ border: '1px solid var(--border)' }}>
          Explain this query
        </button>
      )}
      {open && <ExplainPanel database={database} queryid={id} onClose={() => setOpen(false)} />}
    </div>
  )
}
