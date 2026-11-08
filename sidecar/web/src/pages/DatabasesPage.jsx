import { useState, useEffect, useCallback } from 'react'
import { DatabaseForm } from './databases/DatabaseForm'
import { DatabaseTable } from './databases/DatabaseTable'
import { CSVImport } from './databases/CSVImport'
import { DeleteConfirm } from './databases/DeleteConfirm'
import { FleetFindings } from './databases/FleetFindings'

export function DatabasesPage({ onSelectDatabase }) {
  const [databases, setDatabases] = useState([])
  const [error, setError] = useState(null)
  const [showForm, setShowForm] = useState(false)
  const [editingDB, setEditingDB] = useState(null)
  const [showImport, setShowImport] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState(null)
  const [readOnly, setReadOnly] = useState(false)
  const [writeGuidance, setWriteGuidance] = useState('edit the YAML file')
  const [loading, setLoading] = useState(true)
  const [configFileDBs, setConfigFileDBs] = useState([])
  const [runtimeDBs, setRuntimeDBs] = useState([])
  const [runtimeAvailable, setRuntimeAvailable] = useState(false)
  const [readOnlySource, setReadOnlySource] = useState(null)
  const [inventoryComplete, setInventoryComplete] = useState(false)

  const fetchDatabases = useCallback(async () => {
    setLoading(true)
    setInventoryComplete(false)
    try {
      // Verify ownership before advertising edits. The runtime inventory is
      // independent of managed registration and must survive its failures.
      const [configResult] = await Promise.allSettled([
        fetchJSON('/api/v1/config/global', 'fleet configuration'),
      ])
      const config = configResult.status === 'fulfilled' ? configResult.value : null
      const yaml = config?.read_only === true
      const [runtimeResult, managedResult] = await Promise.allSettled([
        fetchInventory('/api/v1/databases', 'runtime database inventory'),
        config && !yaml
          ? fetchInventory('/api/v1/databases/managed', 'managed database registrations')
          : Promise.resolve([]),
      ])
      const running = runtimeResult.status === 'fulfilled' ? runtimeResult.value : []
      const managed = managedResult.status === 'fulfilled' ? managedResult.value : []
      const canManage = Boolean(config) && !yaml && managedResult.status === 'fulfilled'
      const errors = [configResult, runtimeResult, managedResult]
        .filter(result => result.status === 'rejected')
        .map(result => result.reason.message)
      setRuntimeDBs(running)
      setRuntimeAvailable(runtimeResult.status === 'fulfilled')
      setReadOnly(!canManage)
      setReadOnlySource(yaml ? 'yaml' : canManage ? null : 'unavailable')
      setWriteGuidance(config?.write_guidance || 'edit the YAML file')
      setDatabases(canManage ? managed : running)
      const names = new Set(managed.map(db => db.name))
      setConfigFileDBs(canManage ? running.filter(db => !names.has(db.name)) : [])
      setInventoryComplete(errors.length === 0)
      setError(errors.length ? errors.join('. ') : null)
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

  const inventory = [...new Map([...databases, ...configFileDBs]
    .map(db => [db.name, db])).values()]
  const connected = runtimeDBs.filter(db => db.status?.connected === true).length

  return (
    <div className="space-y-4">
      <div>
        <h2 className="text-sm font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          Fleet
        </h2>
        <p className="text-sm" data-testid="fleet-page-description"
          style={{ color: 'var(--text-secondary)' }}>
          Your monitored Postgres databases, connection health, and registration sources.
          Analyzer findings identify tuning opportunities. SRE investigations produce
          durable cases; supported actions follow the configured approval policy.
        </p>
      </div>

      {error && (
        <div role="alert" data-testid="fleet-load-error" className="text-sm p-3 rounded"
          style={{
            background: 'rgba(239,68,68,0.1)',
            color: '#ef4444',
            border: '1px solid rgba(239,68,68,0.3)',
          }}>
          {error}
          <button className="ml-2 underline" onClick={fetchDatabases}>Retry inventory</button>
          <button className="ml-2 underline"
            onClick={() => setError(null)}>dismiss</button>
        </div>
      )}

      {!loading && (
        <div className="grid gap-3 sm:grid-cols-3" data-testid="fleet-inventory-summary">
          <InventoryMetric label="Known databases" value={inventory.length}
            detail={inventoryComplete ? 'Inventory loaded' : 'Inventory may be incomplete'} />
          <InventoryMetric label="Runtime connections" value={runtimeAvailable ? `${connected} / ${runtimeDBs.length}` : 'Unavailable'}
            detail={runtimeAvailable ? 'Connected / reported by collector' : 'Retry inventory to read connection status'} />
          <InventoryMetric label="Registration" value={readOnlySource === 'yaml' ? 'Config file'
            : readOnlySource === 'unavailable' ? 'Unverified' : 'Config + UI'}
            detail={readOnly ? 'Read-only inventory' : 'UI registration available'} />
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
          {readOnlySource === 'yaml'
            ? <>Fleet membership is read-only in YAML fleet mode. To add, edit,
              or remove databases, {writeGuidance}.</>
            : <>Showing the runtime inventory in read-only mode. Registration settings
              could not be verified; retry inventory to restore management controls.</>}
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

      {!loading && !readOnly && configFileDBs.length > 0 && (
        <div className="space-y-2">
          <p className="text-sm" data-testid="config-file-note"
            style={{ color: 'var(--text-secondary)' }}>
            From the config file (edit the YAML file to change these):
          </p>
          <ReadOnlyFleet databases={configFileDBs}
            testId="config-file-databases" onSelectDatabase={onSelectDatabase} />
        </div>
      )}

      {!loading && (readOnly ? (
        <ReadOnlyFleet databases={databases} onSelectDatabase={onSelectDatabase} />
      ) : databases.length > 0 ? (
        <DatabaseTable databases={databases}
          onEdit={handleEdit}
          onDelete={setDeleteTarget}
          onError={setError} />
      ) : null)}

      {!loading && inventoryComplete && inventory.length === 0 && (
        <div data-testid="fleet-inventory-empty" className="rounded-lg p-8 text-center text-sm" style={{
          background: 'var(--bg-card)', color: 'var(--text-secondary)', border: '1px solid var(--border)',
        }}>
          {readOnly
            ? `No databases are configured. To add a database, ${writeGuidance}.`
            : 'No databases configured yet. Click "Add Database" to get started.'}
        </div>
      )}

      {!loading && <FleetFindings />}
    </div>
  )
}

async function fetchJSON(url, label) {
  const res = await fetch(url, { credentials: 'include' })
  if (!res.ok) throw new Error(`Failed to load ${label}${res.status ? ` (HTTP ${res.status})` : ''}`)
  const data = await res.json()
  if (!data || typeof data !== 'object' || Array.isArray(data)) {
    throw new Error(`Invalid ${label} response`)
  }
  return data
}

async function fetchInventory(url, label) {
  const data = await fetchJSON(url, label)
  if (data.databases === null) return []
  if (!Array.isArray(data.databases) || data.databases.some(db => typeof db?.name !== 'string' || !db.name)) {
    throw new Error(`Invalid ${label} response`)
  }
  return data.databases
}

function InventoryMetric({ label, value, detail }) {
  return (
    <div className="rounded-lg p-4" style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      <p className="text-xs uppercase tracking-wider" style={{ color: 'var(--text-secondary)' }}>{label}</p>
      <p className="text-xl font-semibold font-mono mt-2" style={{ color: 'var(--text-primary)' }}>{value}</p>
      <p className="text-xs mt-1" style={{ color: 'var(--text-secondary)' }}>{detail}</p>
    </div>
  )
}

function ReadOnlyFleet({ databases, testId = 'yaml-fleet-list', onSelectDatabase }) {
  if (!databases.length) return null
  return (
    <div className="rounded-lg overflow-x-auto"
      data-testid={testId}
      style={{
        background: 'var(--bg-card)',
        border: '1px solid var(--border)',
      }}>
      <table className="w-full text-sm">
        <thead><tr style={{ borderBottom: '1px solid var(--border)' }}>
          {['Database', 'Platform / Postgres', 'Connection', 'Trust', 'Open findings', 'Explore'].map(label => (
            <th key={label} className="text-left px-4 py-3 text-xs whitespace-nowrap"
              style={{ color: 'var(--text-secondary)' }}>{label}</th>
          ))}
        </tr></thead>
        <tbody>{databases.map(database => {
          const status = database.status || {}
          const connection = status.connected === true ? 'connected'
            : status.connected === false ? 'disconnected' : 'unknown'
          return (
            <tr key={database.name} style={{ borderBottom: '1px solid var(--border)' }}>
              <td className="px-4 py-4 font-mono font-semibold" style={{ color: 'var(--text-primary)' }}>{database.name}</td>
              <td className="px-4 py-4" style={{ color: 'var(--text-secondary)' }}>
                {status.platform || 'Postgres'}{status.pg_version ? ` / ${status.pg_version}` : ''}
              </td>
              <td className="px-4 py-4"><span className="inline-flex items-center gap-2"
                style={{ color: connection === 'connected' ? 'var(--green)' : connection === 'disconnected' ? 'var(--red)' : 'var(--text-secondary)' }}>
                <span aria-hidden="true" className="w-1.5 h-1.5 rounded-full" style={{ background: 'currentColor' }} />{connection}
              </span></td>
              <td className="px-4 py-4" style={{ color: 'var(--text-secondary)' }}>{status.trust_level || 'unknown'}</td>
              <td className="px-4 py-4 font-mono" style={{ color: 'var(--text-primary)' }}>{status.findings_open ?? '—'}</td>
              <td className="px-4 py-4"><a href="#/database" onClick={() => onSelectDatabase?.(database.name)}
                className="underline whitespace-nowrap" style={{ color: 'var(--accent)' }}>Inspect database</a></td>
            </tr>
          )
        })}</tbody>
      </table>
    </div>
  )
}
