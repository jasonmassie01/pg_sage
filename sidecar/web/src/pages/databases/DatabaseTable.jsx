import { useMemo, useState } from 'react'
import { filterDatabases } from '../../lib/databaseFilter'
import { RuntimeCells } from './RuntimeCells'
import { RUNTIME_HEADERS } from './runtimeStatus'

// TABLE_ROW_LIMIT bounds the rows rendered at once (ux-05e): a fleet of
// 500 shows the first ones and says how to reach the rest (the filter).
export const TABLE_ROW_LIMIT = 100

const muted = { color: 'var(--text-secondary)' }
const cardStyle = { background: 'var(--bg-card)', border: '1px solid var(--border)' }

// FleetFilter is the text filter above the table (name, host or tag).
function FleetFilter({ query, onChange, shown, matched, total }) {
  return (
    <div className="flex flex-wrap items-center gap-3 mb-2">
      <input type="search" aria-label="Filter databases" value={query}
        data-testid="db-filter" placeholder="Filter by name, host or tag"
        onChange={e => onChange(e.target.value)}
        className="px-3 py-1.5 rounded text-sm w-full sm:w-72"
        style={{ background: 'var(--bg-hover)', color: 'var(--text-primary)',
          border: '1px solid var(--border)' }} />
      {matched > shown && (
        <span data-testid="db-filter-count" className="text-xs" style={muted}>
          Showing {shown} of {matched}{matched < total ? ` matching (${total} in all)` : ''};
          {' '}type in the filter to find the rest.
        </span>
      )}
    </div>
  )
}

// useConnectionTest runs a registration's connection test, one at a time.
function useConnectionTest(onError) {
  const [testing, setTesting] = useState(null)
  const [testResult, setTestResult] = useState(null)

  async function handleTest(db) {
    setTesting(db.id)
    setTestResult(null)
    try {
      const res = await fetch(
        `/api/v1/databases/managed/${db.id}/test`,
        {
          method: 'POST',
          credentials: 'include',
          headers: { 'Content-Type': 'application/json' },
        })
      if (!res.ok) throw new Error('Test request failed')
      const data = await res.json()
      setTestResult({ id: db.id, ...data })
    } catch (err) {
      onError(err.message)
    } finally {
      setTesting(null)
    }
  }
  return { testing, testResult, handleTest }
}

// DatabaseTable lists the managed registrations with their runtime state.
// runtimeFor maps a registration to its GET /api/v1/databases row;
// runtimeKnown is false when that inventory could not be read.
export function DatabaseTable({
  databases, onEdit, onDelete, onError,
  runtimeFor = () => undefined, runtimeKnown = true, onSelectDatabase,
}) {
  const { testing, testResult, handleTest } = useConnectionTest(onError)
  const [query, setQuery] = useState('')
  const matched = useMemo(() => filterDatabases(databases, query), [databases, query])
  const shown = matched.length > TABLE_ROW_LIMIT ? matched.slice(0, TABLE_ROW_LIMIT) : matched

  if (databases.length === 0) {
    return (
      <div className="rounded-lg p-8 text-center text-sm"
        style={{
          background: 'var(--bg-card)',
          color: 'var(--text-secondary)',
          border: '1px solid var(--border)',
        }}>
        No databases configured yet. Click "Add Database" to get started.
      </div>
    )
  }

  const rowProps = {
    onEdit, onDelete, onTest: handleTest, testing, testResult, runtimeFor, runtimeKnown,
    onSelectDatabase,
  }
  return (
    <div>
      <FleetFilter query={query} onChange={setQuery} shown={shown.length}
        matched={matched.length} total={databases.length} />
      {matched.length === 0 ? (
        <div data-testid="db-filter-empty" role="status"
          className="rounded-lg p-6 text-center text-sm" style={{ ...muted, ...cardStyle }}>
          No database matches &ldquo;{query.trim()}&rdquo;. Clear the filter to see all
          {' '}{databases.length}.
        </div>
      ) : <FleetRows databases={shown} {...rowProps} />}
    </div>
  )
}

function FleetRows({ databases, testing, testResult, runtimeFor, ...rest }) {
  return (
    <div className="rounded-lg overflow-x-auto" style={cardStyle}>
      <table className="w-full text-sm" data-testid="databases-table">
        <thead>
          <tr style={{ borderBottom: '1px solid var(--border)' }}>
            {['Name', 'Host', 'Trust Level', 'Execution Mode', 'Registration',
              ...RUNTIME_HEADERS, ''].map(h => (
              <th key={h} className="text-left px-4 py-2 text-xs whitespace-nowrap"
                style={muted}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {databases.map(db => (
            <DatabaseRow key={db.id} db={db} {...rest}
              testing={testing === db.id}
              testResult={testResult?.id === db.id ? testResult : null}
              runtime={runtimeFor(db)} />
          ))}
        </tbody>
      </table>
    </div>
  )
}

function DatabaseRow({
  db, onEdit, onDelete, onTest, testing, testResult,
  runtime, runtimeKnown, onSelectDatabase,
}) {
  const badge = (text, color) => (
    <span className="px-2 py-0.5 rounded text-xs"
      style={{
        background: `${color}20`,
        color,
        border: `1px solid ${color}40`,
      }}>{text}</span>
  )

  const trustColors = {
    observation: '#3b82f6',
    advisory: '#f59e0b',
    autonomous: '#10b981',
  }
  const modeColors = {
    auto: '#10b981',
    approval: '#f59e0b',
    manual: '#6b7280',
  }

  return (
    <tr data-testid="db-row"
      style={{ borderBottom: '1px solid var(--border)' }}>
      <td className="px-4 py-2"
        style={{ color: 'var(--text-primary)' }}>
        {db.name}
      </td>
      <td className="px-4 py-2"
        style={{ color: 'var(--text-secondary)' }}>
        {db.host}:{db.port}/{db.database_name}
      </td>
      <td className="px-4 py-2">
        {badge(db.trust_level, trustColors[db.trust_level] || '#6b7280')}
      </td>
      <td className="px-4 py-2">
        {badge(db.execution_mode, modeColors[db.execution_mode] || '#6b7280')}
      </td>
      <td className="px-4 py-2 whitespace-nowrap" data-testid="db-registration">
        {testResult && (testResult.status === 'ok'
          ? badge('test: connected', '#10b981')
          : badge('test: error', '#ef4444'))}
        {!testResult && !db.enabled && badge('disabled', '#6b7280')}
      </td>
      <RuntimeCells runtime={runtime} runtimeKnown={runtimeKnown}
        registeredTrust={db.trust_level} onSelectDatabase={onSelectDatabase} />
      <td className="px-4 py-2 text-right">
        <div className="flex gap-2 justify-end">
          <button onClick={() => onTest(db)} disabled={testing}
            data-testid="db-test-button"
            className="px-2 py-1 rounded text-xs"
            style={{ color: 'var(--accent)' }}>
            {testing ? 'Testing...' : 'Test'}
          </button>
          <button onClick={() => onEdit(db)}
            data-testid="db-edit-button"
            className="px-2 py-1 rounded text-xs"
            style={{ color: 'var(--text-secondary)' }}>
            Edit
          </button>
          <button onClick={() => onDelete(db)}
            data-testid="db-delete-button"
            className="px-2 py-1 rounded text-xs"
            style={{ color: '#ef4444' }}>
            Delete
          </button>
        </div>
      </td>
    </tr>
  )
}
