import { useState } from 'react'
import { addTag, ariaSort, nextSort } from './fleetList'

// FleetControls: the fleet table's server-side search box, its tag filter
// chips, its sortable headers and the bulk-action bar for the selected rows
// (ux-10a).

const muted = { color: 'var(--text-secondary)' }
const inputStyle = {
  background: 'var(--bg-hover)', color: 'var(--text-primary)', border: '1px solid var(--border)',
}
const chipStyle = {
  background: 'var(--bg-card)', color: 'var(--text-primary)', border: '1px solid var(--border)',
}

// FleetSearch is the search box (name or host) and the tag filter: Enter
// in the tag box adds a chip, a chip's button removes it.
export function FleetSearch({ query, onQuery, tags, onTags }) {
  const [draft, setDraft] = useState('')
  function onTagKey(e) {
    if (e.key !== 'Enter') return
    e.preventDefault()
    onTags(addTag(tags, draft))
    setDraft('')
  }
  return (
    <div className="flex flex-wrap items-center gap-3 mb-2">
      <input type="search" aria-label="Search databases" value={query}
        data-testid="db-filter" placeholder="Search by name or host"
        onChange={e => onQuery(e.target.value)}
        className="px-3 py-1.5 rounded text-sm w-full sm:w-72" style={inputStyle} />
      <input type="text" aria-label="Add tag filter" value={draft}
        data-testid="db-tag-input" placeholder="Tag (tier=prod or a bare tag), Enter"
        onChange={e => setDraft(e.target.value)} onKeyDown={onTagKey}
        className="px-3 py-1.5 rounded text-sm w-full sm:w-64" style={inputStyle} />
      {tags.length > 0 && (
        <ul className="flex flex-wrap gap-2" aria-label="Tag filters" data-testid="db-tag-chips">
          {tags.map(tag => (
            <li key={tag} className="px-2 py-0.5 rounded text-xs flex items-center gap-1"
              style={chipStyle} data-testid="db-tag-chip">
              {tag}
              <button type="button" aria-label={`Remove tag filter ${tag}`}
                onClick={() => onTags(tags.filter(t => t !== tag))}>Ã—</button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

// FleetCount says how many rows are shown of how many match, and how many
// of the matches come from the config file (listed in their own section).
export function FleetCount({ shown, total, configFile }) {
  if (!Number.isFinite(total)) return null
  return (
    <p data-testid="db-filter-count" className="text-xs mb-2" style={muted}>
      Showing {shown} of {total} matching
      {configFile > 0 && <>; {configFile} from the config file are listed above</>}.
    </p>
  )
}

// BulkBar acts on the selected registrations through the per-row routes.
export function BulkBar({ count, running, summary, onTest, onClear }) {
  if (count === 0 && !summary) return null
  return (
    <div role="region" aria-label="Bulk actions" data-testid="db-bulk-bar"
      className="flex flex-wrap items-center gap-3 mb-2 text-sm">
      {count > 0 && <span data-testid="db-bulk-count">{count} selected</span>}
      {count > 0 && (
        <button type="button" onClick={onTest} disabled={running}
          data-testid="db-bulk-test" className="px-2 py-1 rounded text-xs"
          style={{ color: 'var(--accent)', border: '1px solid var(--border)' }}>
          {running ? 'Testingâ€¦' : 'Test connection'}
        </button>
      )}
      {count > 0 && (
        <button type="button" onClick={onClear} data-testid="db-bulk-clear"
          className="px-2 py-1 rounded text-xs" style={muted}>Clear selection</button>
      )}
      {summary && <span role="status" data-testid="db-bulk-summary" style={muted}>{summary}</span>}
    </div>
  )
}

// NoMatch is the empty state of a search or tag filter with no result.
export function NoMatch({ onClear }) {
  return (
    <div data-testid="db-filter-empty" role="status"
      className="rounded-lg p-6 text-center text-sm"
      style={{ ...muted, background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      No databases match; <button type="button" className="underline"
        data-testid="db-filter-clear" onClick={onClear}>clear filters</button>
    </div>
  )
}

// SortHeader is a column title: a button that sorts the server list, with
// aria-sort, for the columns in FLEET_SORTS, plain text for the others.
export function SortHeader({ label, sort, onSort }) {
  const state = ariaSort(sort, label)
  if (!state) {
    return <th className="text-left px-4 py-2 text-xs whitespace-nowrap" style={muted}>{label}</th>
  }
  return (
    <th aria-sort={state} className="text-left px-4 py-2 text-xs whitespace-nowrap" style={muted}>
      <button type="button" data-testid={`db-sort-${label}`}
        onClick={() => onSort(nextSort(sort, label))}>
        {label}{state === 'ascending' ? ' ▲' : state === 'descending' ? ' ▼' : ''}
      </button>
    </th>
  )
}
