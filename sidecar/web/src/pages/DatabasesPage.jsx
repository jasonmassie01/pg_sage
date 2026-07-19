import { useState, useEffect, useCallback } from 'react'
import { DatabaseForm } from './databases/DatabaseForm'
import { DatabaseTable } from './databases/DatabaseTable'
import { CSVImport } from './databases/CSVImport'
import { DeleteConfirm } from './databases/DeleteConfirm'

export function DatabasesPage() {
  const [databases, setDatabases] = useState([])
  const [error, setError] = useState(null)
  const [showForm, setShowForm] = useState(false)
  const [editingDB, setEditingDB] = useState(null)
  const [showImport, setShowImport] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState(null)
  const [readOnly, setReadOnly] = useState(false)
  const [writeGuidance, setWriteGuidance] = useState('edit the YAML file')
  const [loading, setLoading] = useState(true)

  const fetchDatabases = useCallback(async () => {
    setLoading(true)
    try {
      const configRes = await fetch('/api/v1/config/global', {
        credentials: 'include',
      })
      if (configRes.ok) {
        const configData = await configRes.json()
        if (configData.read_only === true) {
          const fleetRes = await fetch('/api/v1/databases', {
            credentials: 'include',
          })
          if (!fleetRes.ok) throw new Error('Failed to load YAML fleet')
          const fleetData = await fleetRes.json()
          setReadOnly(true)
          setWriteGuidance(
            configData.write_guidance || 'edit the YAML file'
          )
          setDatabases(fleetData.databases || [])
          setError(null)
          return
        }
      }
      const res = await fetch('/api/v1/databases/managed', {
        credentials: 'include',
      })
      if (!res.ok) throw new Error('Failed to load databases')
      const data = await res.json()
      setReadOnly(false)
      setDatabases(data.databases || [])
      setError(null)
    } catch (err) {
      setError(err.message)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { fetchDatabases() }, [fetchDatabases])

  function handleEdit(db) {
    setEditingDB(db)
    setShowForm(true)
  }

  function handleFormClose() {
    setShowForm(false)
    setEditingDB(null)
    fetchDatabases()
  }

  function handleImportClose() {
    setShowImport(false)
    fetchDatabases()
  }

  return (
    <div className="space-y-4">
      <div>
        <h2 className="text-sm font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          Fleet
        </h2>
        <p className="text-sm" data-testid="fleet-page-description"
          style={{ color: 'var(--text-secondary)' }}>
          Register the Postgres fleet pg_sage should watch. Collector and
          analyzer results become cases, and action-ready cases flow into
          actions for approval, execution, and audit.
        </p>
      </div>

      {error && (
        <div className="text-sm p-3 rounded"
          style={{
            background: 'rgba(239,68,68,0.1)',
            color: '#ef4444',
            border: '1px solid rgba(239,68,68,0.3)',
          }}>
          {error}
          <button className="ml-2 underline"
            onClick={() => setError(null)}>dismiss</button>
        </div>
      )}

      {loading && (
        <div data-testid="fleet-loading" className="text-sm p-3"
          style={{ color: 'var(--text-secondary)' }}>
          Loading fleet configuration...
        </div>
      )}

      {!loading && readOnly && (
        <div
          data-testid="fleet-read-only"
          className="text-sm p-3 rounded"
          style={{
            background: 'var(--bg-card)',
            color: 'var(--yellow)',
            border: '1px solid var(--yellow)',
          }}
        >
          Fleet membership is read-only in YAML fleet mode. To add, edit,
          or remove databases, {writeGuidance}.
        </div>
      )}

      {!loading && !readOnly && <div className="flex gap-3">
        <button onClick={() => { setEditingDB(null); setShowForm(true) }}
          data-testid="add-database-button"
          className="px-4 py-1.5 rounded text-sm font-medium"
          style={{ background: 'var(--accent)', color: '#fff' }}>
          Add Database
        </button>
        <button onClick={() => setShowImport(true)}
          data-testid="import-csv-button"
          className="px-4 py-1.5 rounded text-sm font-medium"
          style={{
            background: 'var(--bg-card)',
            color: 'var(--text-primary)',
            border: '1px solid var(--border)',
          }}>
          Import CSV
        </button>
      </div>}

      {showForm && (
        <DatabaseForm db={editingDB} onClose={handleFormClose}
          onError={setError} />
      )}

      {showImport && (
        <CSVImport onClose={handleImportClose}
          onError={setError} />
      )}

      {deleteTarget && (
        <DeleteConfirm db={deleteTarget}
          onConfirm={async () => {
            try {
              const res = await fetch(
                `/api/v1/databases/managed/${deleteTarget.id}`,
                { method: 'DELETE', credentials: 'include' })
              if (!res.ok) throw new Error('Failed to delete')
              setDeleteTarget(null)
              fetchDatabases()
            } catch (err) {
              setError(err.message)
              setDeleteTarget(null)
            }
          }}
          onCancel={() => setDeleteTarget(null)} />
      )}

      {!loading && (readOnly ? (
        <ReadOnlyFleet databases={databases} />
      ) : (
        <DatabaseTable databases={databases}
          onEdit={handleEdit}
          onDelete={setDeleteTarget}
          onError={setError} />
      ))}
    </div>
  )
}

function ReadOnlyFleet({ databases }) {
  return (
    <div className="rounded-lg divide-y"
      data-testid="yaml-fleet-list"
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      {databases.map(database => (
        <div key={database.id || database.database_id || database.name}
          className="flex items-center justify-between px-4 py-3 text-sm">
          <span style={{ color: 'var(--text-primary)' }}>
            {database.name}
          </span>
          <span style={{ color: 'var(--text-secondary)' }}>
            {database.status?.trust_level || 'unknown'} ·{' '}
            {database.status?.connected ? 'connected' : 'disconnected'}
          </span>
        </div>
      ))}
    </div>
  )
}
