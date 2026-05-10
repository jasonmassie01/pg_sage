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

  const fetchDatabases = useCallback(async () => {
    try {
      const res = await fetch('/api/v1/databases/managed', {
        credentials: 'include',
      })
      if (!res.ok) throw new Error('Failed to load databases')
      const data = await res.json()
      setDatabases(data.databases || [])
    } catch (err) {
      setError(err.message)
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

      <div className="flex gap-3">
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
      </div>

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

      <DatabaseTable databases={databases}
        onEdit={handleEdit}
        onDelete={setDeleteTarget}
        onError={setError} />
    </div>
  )
}
