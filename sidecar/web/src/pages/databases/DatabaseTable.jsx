import { useCallback, useMemo, useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { useCursorPages } from '../../lib/listPaging'
import { LoadMore } from '../../components/LoadMore'
import { BulkBar, FleetCount, FleetSearch, NoMatch, SortHeader } from './FleetControls'
import { DatabaseRow } from './DatabaseRow'
import { fleetRows } from './fleetList'
import { useFleetView } from './fleetView'
import { useBulkTag } from './bulkTag'
import { RUNTIME_HEADERS } from './runtimeStatus'

// DatabaseTable is the fleet table of the managed registrations (ux-10a):
// the sidecar searches (name or host), filters by tag, sorts and pages it
// (GET /api/v1/databases?q&tag&sort&order&limit), and rows can be selected
// for a bulk connection test or tag through the per-row routes. databases are the
// registrations; runtimeFor maps one to its row of the whole runtime
// inventory, runtimeKnown is false when that inventory could not be read.

const cardStyle = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
const muted = { color: 'var(--text-secondary)' }

async function testConnection(db) {
  const res = await fetch(`/api/v1/databases/managed/${db.id}/test`, {
    method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' },
  })
  if (!res.ok) throw new Error(`Test request failed for ${db.name}`)
  return { id: db.id, ...(await res.json()) }
}

// useConnectionTests runs connection tests, one at a time, and keeps each
// registration's last result by id.
function useConnectionTests(onError) {
  const [testing, setTesting] = useState(null)
  const [results, setResults] = useState({})
  const [summary, setSummary] = useState('')
  const run = useCallback(async dbs => {
    let ok = 0
    let failed = 0
    setSummary('')
    for (const db of dbs) {
      setTesting(db.id)
      try {
        const result = await testConnection(db)
        setResults(prev => ({ ...prev, [db.id]: result }))
        if (result.status === 'ok') ok += 1
        else failed += 1
      } catch (err) {
        failed += 1
        onError(err.message)
      }
    }
    setTesting(null)
    if (dbs.length > 1) setSummary(`Tested ${dbs.length}: ${ok} connected, ${failed} failed`)
  }, [onError])
  return { testing, results, summary, run }
}

export function DatabaseTable({
  databases, onEdit, onDelete, onError,
  runtimeFor = () => undefined, runtimeKnown = true, onSelectDatabase, onChanged = () => {},
  initialTags,
}) {
  const view = useFleetView(initialTags)
  const list = useAPI(databases.length ? view.url : null, 30000)
  const paging = useCursorPages(list.data, view.url, 'databases', { idOf: row => row?.name })
  const tests = useConnectionTests(onError)
  const bulkTag = useBulkTag(onError, onChanged)
  const rows = useMemo(() => fleetRows(databases, paging.rows), [databases, paging.rows])

  if (databases.length === 0) {
    return (
      <div className="rounded-lg p-8 text-center text-sm" style={{ ...muted, ...cardStyle }}>
        No databases configured yet. Click &quot;Add Database&quot; to get started.
      </div>
    )
  }
  const listFailed = Boolean(list.error) && !list.data
  const managedRows = listFailed
    ? databases.map(reg => ({ key: reg.name, registration: reg, runtime: runtimeFor(reg) }))
    : rows.filter(r => r.registration)
  const notRunning = listFailed || view.filtered || !runtimeKnown ? []
    : databases.filter(reg => !runtimeFor(reg))
  const rowProps = {
    onEdit, onDelete, onSelectDatabase, runtimeKnown: listFailed ? runtimeKnown : true,
    tests, selected: view.selected, onToggle: view.toggle,
  }
  const selectedRegs = databases.filter(reg => view.selected.has(reg.id))
  return (
    <div>
      <FleetSearch query={view.query} onQuery={view.setQuery} tags={view.tags}
        onTags={view.setTags} />
      <BulkBar count={selectedRegs.length} running={tests.testing !== null}
        summary={tests.summary} onTest={() => tests.run(selectedRegs)}
        onClear={() => view.setSelected(new Set())}
        tag={{ ...bulkTag, onRun: raw => bulkTag.run(selectedRegs, raw) }} />
      <FleetStatusLine list={list} listFailed={listFailed} shown={managedRows.length}
        configFile={rows.length - rows.filter(r => r.registration).length} />
      <FleetBody list={list} listFailed={listFailed} filtered={view.filtered}
        onClear={view.clear} rows={managedRows} sort={view.sort} onSort={view.setSort}
        {...rowProps} />
      {!listFailed && <div className="mt-2"><LoadMore paging={paging} testId="db-load-more" /></div>}
      {notRunning.length > 0 && (
        <NotRunning registrations={notRunning} {...rowProps} runtimeKnown />
      )}
    </div>
  )
}

function FleetStatusLine({ list, listFailed, shown, configFile }) {
  if (listFailed) {
    return (
      <p role="status" data-testid="db-list-error" className="text-xs mb-2"
        style={{ color: 'var(--red)' }}>
        Search, sort and paging are unavailable ({list.error}); showing every registration.
      </p>
    )
  }
  if (!list.data) return null
  return <FleetCount shown={shown} total={list.data.total} configFile={configFile} />
}

// FleetBody is the table once its first page arrived (or the fallback
// list), the empty state of a filter with no match, or a loading line.
function FleetBody({ list, listFailed, filtered, onClear, ...rest }) {
  if (!listFailed && !list.data) {
    return <p data-testid="db-list-loading" className="text-sm" style={muted}>Loading databasesâ€¦</p>
  }
  if (!listFailed && list.data.total === 0 && filtered) return <NoMatch onClear={onClear} />
  return <FleetRows {...rest} />
}

function SelectAll({ rows, selected, onToggle }) {
  const ids = rows.map(r => r.registration.id)
  const all = ids.length > 0 && ids.every(id => selected.has(id))
  return (
    <input type="checkbox" aria-label="Select all shown databases" checked={all}
      data-testid="db-select-all"
      onChange={() => ids.filter(id => all || !selected.has(id)).forEach(onToggle)} />
  )
}

const HEADERS = ['Name', 'Host', 'Trust Level', 'Execution Mode', 'Registration',
  ...RUNTIME_HEADERS]

function FleetRows({ rows, sort, onSort, ...rest }) {
  return (
    <div className="rounded-lg overflow-x-auto" style={cardStyle}>
      <table className="w-full text-sm" data-testid="databases-table">
        <thead>
          <tr style={{ borderBottom: '1px solid var(--border)' }}>
            <th scope="col" className="px-2 py-2"><SelectAll rows={rows} selected={rest.selected}
              onToggle={rest.onToggle} /></th>
            {HEADERS.map(h => <SortHeader key={h} label={h} sort={sort} onSort={onSort} />)}
            <th scope="col"><span className="sr-only">Actions</span></th>
          </tr>
        </thead>
        <tbody>
          {rows.map(row => (
            <FleetRow key={row.key} db={row.registration} runtime={row.runtime} {...rest} />
          ))}
        </tbody>
      </table>
    </div>
  )
}

// FleetRow hands a row only its own selection and test state, so the
// memoized DatabaseRow re-renders only the rows that changed.
function FleetRow({ db, tests, selected, ...rest }) {
  return <DatabaseRow db={db} checked={selected.has(db.id)} testing={tests.testing === db.id}
    result={tests.results[db.id]} onTest={tests.run} {...rest} />
}

// NotRunning lists the registrations the runtime does not report
// (disabled, or not started); the server list cannot page them.
function NotRunning({ registrations, ...rest }) {
  return (
    <div className="mt-4 space-y-2" data-testid="db-not-running">
      <p className="text-sm" style={muted}>Registered but not running:</p>
      <div className="rounded-lg overflow-x-auto" style={cardStyle}>
        <table className="w-full text-sm">
          <tbody>
            {registrations.map(reg => (
              <FleetRow key={reg.id} db={reg} runtime={undefined} {...rest} />
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
