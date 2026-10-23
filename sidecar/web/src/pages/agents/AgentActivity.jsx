// An agent's activity over the last 24 hours (spec §8.3, G1-10): live
// sessions, the statements pg_stat_statements attributes to its roles, and
// its audited agent_query calls, per database. When attribution may have
// been dropped the audit rows are the record.
import { useEffect, useState } from 'react'
import { agentActivity } from './agentsApi'
import { ErrorBox, Section, muted, when } from './ui'

function Attribution({ db, a }) {
  if (!a || (a.complete && !a.dropped)) return null
  return (
    <p data-testid={`agent-activity-dropped-${db}`} className="text-xs mb-2"
      style={{ color: '#f59e0b' }}>
      Statement attribution may be incomplete
      {a.reason ? `: ${a.reason}` : ''}. The audited queries below are the record.
    </p>
  )
}

function DatabaseActivity({ item }) {
  const db = item.database
  return (
    <div data-testid={`agent-activity-${db}`} className="mb-3 text-sm"
      style={{ color: 'var(--text-primary)' }}>
      <h4 className="font-semibold mb-1">{db}</h4>
      <Attribution db={db} a={item.attribution} />
      <p style={muted}>{item.sessions.length} live sessions</p>
      <ul className="ml-3 mb-1">
        {item.sessions.map(s => (
          <li key={s.pid}>pid {s.pid} {s.role} {s.state} since {when(s.query_start)}</li>
        ))}
      </ul>
      <p style={muted}>Statements</p>
      <ul className="ml-3 mb-1">
        {item.statements.map(s => (
          <li key={`${s.role}-${s.query_id}`}>
            <code className="text-xs">{s.query}</code> ({s.calls} calls, {s.rows} rows)
          </li>
        ))}
      </ul>
      <p style={muted}>Audited queries</p>
      <ul className="ml-3">
        {item.queries.map(q => (
          <li key={q.id}>
            {when(q.at)} {q.verdict}{q.reason ? ` (${q.reason}${q.step ? `, ${q.step}` : ''})`
              : ''}{q.row_count != null ? `, ${q.row_count} rows` : ''}
          </li>
        ))}
      </ul>
    </div>
  )
}

export function AgentActivity({ agent }) {
  const [state, setState] = useState({ items: null, error: null })
  useEffect(() => {
    let live = true
    agentActivity(agent.id)
      .then(data => { if (live) setState({ items: data?.items || [], error: null }) })
      .catch(err => { if (live) setState({ items: [], error: err }) })
    return () => { live = false }
  }, [agent.id])
  return (
    <Section title="Activity (last 24 hours)" testId="agent-activity">
      <ErrorBox testId="agent-activity-error" error={state.error} />
      {state.items && state.items.length === 0 && !state.error && (
        <p data-testid="agent-activity-empty" className="text-sm" style={muted}>
          No activity recorded.
        </p>
      )}
      {(state.items || []).map(item => <DatabaseActivity key={item.database}
        item={item} />)}
    </Section>
  )
}
