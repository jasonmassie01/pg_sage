import { useAPI } from '../../hooks/useAPI'
import { useCursorPages } from '../../lib/listPaging'
import { LoadMore } from '../../components/LoadMore'
import { FleetCount, FleetSearch, NoMatch, SortHeader } from './FleetControls'
import { RuntimeCells } from './RuntimeCells'
import { useFleetView } from './fleetView'
import { RUNTIME_HEADERS } from './runtimeStatus'

// ReadOnlyFleet lists runtime databases the page cannot manage: the
// config-file section, and the whole fleet in YAML fleet mode. With sort
// and onSort its headers sort the server list.

const muted = { color: 'var(--text-secondary)' }
const cardStyle = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
const HEADERS = ['Database', 'Platform / Postgres', ...RUNTIME_HEADERS]

export function ReadOnlyFleet({ databases, testId = 'yaml-fleet-list', onSelectDatabase,
  sort, onSort }) {
  if (!databases.length) return null
  return (
    <div className="rounded-lg overflow-x-auto" data-testid={testId} style={cardStyle}>
      <table className="w-full text-sm">
        <thead><tr style={{ borderBottom: '1px solid var(--border)' }}>
          {HEADERS.map(label => (onSort
            ? <SortHeader key={label} label={label} sort={sort} onSort={onSort} />
            : <th key={label} className="text-left px-4 py-3 text-xs whitespace-nowrap"
              style={muted}>{label}</th>))}
        </tr></thead>
        <tbody>{databases.map(database => (
          <ReadOnlyRow key={database.name} database={database}
            onSelectDatabase={onSelectDatabase} />
        ))}</tbody>
      </table>
    </div>
  )
}

function ReadOnlyRow({ database, onSelectDatabase }) {
  const status = database.status || {}
  return (
    <tr style={{ borderBottom: '1px solid var(--border)' }}>
      <td className="px-4 py-4 font-mono font-semibold"
        style={{ color: 'var(--text-primary)' }}>{database.name}</td>
      <td className="px-4 py-4" style={muted}>
        {status.platform || 'Postgres'}{status.pg_version ? ` / ${status.pg_version}` : ''}
      </td>
      <RuntimeCells runtime={database} onSelectDatabase={onSelectDatabase} />
    </tr>
  )
}

// ReadOnlyFleetTable is the read-only fleet at fleet scale (ux-10a): the
// sidecar searches, tag-filters, sorts and pages it like the managed table.
// It has no row selection: the bulk actions need an API-managed
// registration, and the page's read-only note says to edit the YAML file.
// databases is the whole runtime inventory, shown if the list fails.
export function ReadOnlyFleetTable({ databases, onSelectDatabase, initialTags }) {
  const view = useFleetView(initialTags)
  const list = useAPI(databases.length ? view.url : null, 30000)
  const paging = useCursorPages(list.data, view.url, 'databases', { idOf: row => row?.name })
  if (!databases.length) return null
  const listFailed = Boolean(list.error) && !list.data
  return (
    <div data-testid="read-only-fleet-table">
      <FleetSearch query={view.query} onQuery={view.setQuery} tags={view.tags}
        onTags={view.setTags} />
      <ReadOnlyBody list={list} listFailed={listFailed} view={view} paging={paging}
        databases={databases} onSelectDatabase={onSelectDatabase} />
    </div>
  )
}

function ReadOnlyBody({ list, listFailed, view, paging, databases, onSelectDatabase }) {
  if (listFailed) {
    return (
      <>
        <p role="status" data-testid="db-list-error" className="text-xs mb-2"
          style={{ color: 'var(--red)' }}>
          Search, sort and paging are unavailable ({list.error}); showing every database.
        </p>
        <ReadOnlyFleet databases={databases} onSelectDatabase={onSelectDatabase} />
      </>
    )
  }
  if (!list.data) {
    return <p data-testid="db-list-loading" className="text-sm" style={muted}>Loading databases…</p>
  }
  if (list.data.total === 0 && view.filtered) return <NoMatch onClear={view.clear} />
  return (
    <>
      <FleetCount shown={paging.rows.length} total={list.data.total} configFile={0} />
      <ReadOnlyFleet databases={paging.rows} onSelectDatabase={onSelectDatabase}
        sort={view.sort} onSort={view.setSort} />
      <div className="mt-2"><LoadMore paging={paging} testId="db-load-more" /></div>
    </>
  )
}
