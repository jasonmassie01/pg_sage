import { memo, useState } from 'react'
import { WhyNotPanel } from '../../components/WhyNotPanel'
import { RuntimeCells } from './RuntimeCells'

// DatabaseRow is one managed registration of the fleet table with its
// runtime state, its selection box and its Why not?, Test, Edit and Delete
// controls. Why not? opens the why-not chain for the database in a row of
// its own under the row (ux-08b).

const TRUST_COLORS = { observation: '#3b82f6', advisory: '#f59e0b', autonomous: '#10b981' }
const MODE_COLORS = { auto: '#10b981', approval: '#f59e0b', manual: '#6b7280' }

function Badge({ text, color }) {
  return (
    <span className="px-2 py-0.5 rounded text-xs"
      style={{ background: `${color}20`, color, border: `1px solid ${color}40` }}>{text}</span>
  )
}

function RowActions({ db, testing, onTest, onEdit, onDelete, whyNot, onWhyNot }) {
  return (
    <td className="px-4 py-2 text-right">
      <div className="flex gap-2 justify-end">
        <button type="button" aria-expanded={whyNot} onClick={onWhyNot}
          className="px-2 py-1 rounded text-xs" style={{ color: 'var(--accent)' }}>
          Why not?
        </button>
        <button onClick={() => onTest([db])} disabled={testing}
          data-testid="db-test-button" className="px-2 py-1 rounded text-xs"
          style={{ color: 'var(--accent)' }}>
          {testing ? 'Testing...' : 'Test'}
        </button>
        <button onClick={() => onEdit(db)} data-testid="db-edit-button"
          className="px-2 py-1 rounded text-xs" style={{ color: 'var(--text-secondary)' }}>
          Edit
        </button>
        <button onClick={() => onDelete(db)} data-testid="db-delete-button"
          className="px-2 py-1 rounded text-xs" style={{ color: '#ef4444' }}>
          Delete
        </button>
      </div>
    </td>
  )
}

// DatabaseRow is memoized: a page load or a selection re-renders only the
// rows whose props changed (500 rows stay responsive, ux-10a).
export const DatabaseRow = memo(function DatabaseRow({
  db, runtime, runtimeKnown, onSelectDatabase, onEdit, onDelete,
  checked, testing, result, onTest, onToggle, columns,
}) {
  const [whyNot, setWhyNot] = useState(false)
  return (<>
    <tr data-testid="db-row" style={{ borderBottom: '1px solid var(--border)' }}>
      <td className="px-2 py-2">
        <input type="checkbox" aria-label={`Select ${db.name}`} data-testid="db-select"
          checked={checked} onChange={() => onToggle(db.id)} />
      </td>
      <td className="px-4 py-2" style={{ color: 'var(--text-primary)' }}>{db.name}</td>
      <td className="px-4 py-2" style={{ color: 'var(--text-secondary)' }}>
        {db.host}:{db.port}/{db.database_name}
      </td>
      <td className="px-4 py-2">
        <Badge text={db.trust_level} color={TRUST_COLORS[db.trust_level] || '#6b7280'} />
      </td>
      <td className="px-4 py-2">
        <Badge text={db.execution_mode} color={MODE_COLORS[db.execution_mode] || '#6b7280'} />
      </td>
      <td className="px-4 py-2 whitespace-nowrap" data-testid="db-registration">
        {result && (result.status === 'ok'
          ? <Badge text="test: connected" color="#10b981" />
          : <Badge text="test: error" color="#ef4444" />)}
        {!result && !db.enabled && <Badge text="disabled" color="#6b7280" />}
      </td>
      <RuntimeCells runtime={runtime} runtimeKnown={runtimeKnown}
        registeredTrust={db.trust_level} onSelectDatabase={onSelectDatabase} />
      <RowActions db={db} testing={testing} onTest={onTest} onEdit={onEdit} onDelete={onDelete}
        whyNot={whyNot} onWhyNot={() => setWhyNot(open => !open)} />
    </tr>
    {whyNot && (
      <tr style={{ borderBottom: '1px solid var(--border)' }}>
        <td colSpan={columns} className="px-4 pb-3"><WhyNotPanel database={db.name} /></td>
      </tr>
    )}
  </>)
})
