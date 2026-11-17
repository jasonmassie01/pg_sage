import { useId, useMemo, useState } from 'react'
import { filterDatabases } from '../lib/databaseFilter'

// PICKER_FILTER_FROM is the fleet size from which the scope picker adds
// its find box (ux-05e, win 19); a smaller fleet keeps the plain select.
export const PICKER_FILTER_FROM = 6

const control = {
  background: 'var(--bg-hover)',
  color: 'var(--text-primary)',
  border: '1px solid var(--border)',
}

// pickFrom is the database a find-box value selects: on Enter, the exact
// name (any case) or the single match; while typing, only an exact name
// that no other name extends (so "orders" waits while "orders_archive"
// exists). '' when it selects none.
function pickFrom(databases, value, enter) {
  const names = databases.map(db => db.name)
  if (!enter) {
    const extended = names.some(n => n !== value && n.startsWith(value))
    return names.includes(value) && !extended ? value : ''
  }
  const low = value.trim().toLowerCase()
  const exact = names.find(n => n.toLowerCase() === low)
  if (exact) return exact
  const matches = filterDatabases(databases, value)
  return matches.length === 1 ? matches[0].name : ''
}

// DatabasePicker is the header scope select. For a fleet large enough to
// scroll, a find box over the same names narrows the options; the select
// always keeps "All Databases" and the selected scope.
export function DatabasePicker({ databases, selected, onSelect, ...rest }) {
  // A plain id: useId's punctuation is not a valid CSS selector.
  const listID = `database-find-${useId().replace(/[^A-Za-z0-9_-]/g, '')}`
  const [query, setQuery] = useState('')
  const filterable = databases.length >= PICKER_FILTER_FROM
  const matched = useMemo(() => filterDatabases(databases, query), [databases, query])
  const options = filterable && query.trim() ? matched : databases
  const keepSelected = selected && selected !== 'all' &&
    !options.some(db => db.name === selected)

  function choose(value, enter) {
    const name = pickFrom(databases, value, enter)
    if (!name) return
    setQuery('')
    onSelect(name)
  }

  return (
    <div className="flex items-center gap-2">
      {filterable && <FindBox listID={listID} databases={databases} query={query}
        noMatch={Boolean(query.trim()) && matched.length === 0}
        onChange={value => { setQuery(value); choose(value, false) }}
        onEnter={() => choose(query, true)} />}
      <select aria-label="Database scope" value={selected}
        onChange={e => onSelect(e.target.value)}
        {...rest}
        className="px-3 py-1.5 rounded text-sm" style={control}>
        <option value="all">All Databases</option>
        {keepSelected && <option value={selected}>{selected}</option>}
        {options.map(db => (
          <option key={db.name} value={db.name}>{db.name}</option>
        ))}
      </select>
    </div>
  )
}

function FindBox({ listID, databases, query, noMatch, onChange, onEnter }) {
  return (
    <>
      <input aria-label="Find database" list={listID} value={query}
        placeholder="Find database" data-testid="database-picker-find"
        onChange={e => onChange(e.target.value)}
        onKeyDown={e => {
          if (e.key === 'Enter') {
            e.preventDefault()
            onEnter()
          }
        }}
        className="px-2 py-1.5 rounded text-sm w-36" style={control} />
      <datalist id={listID}>
        {databases.map(db => <option key={db.name} value={db.name} />)}
      </datalist>
      {noMatch && (
        <span data-testid="database-picker-nomatch" role="status" className="text-xs"
          style={{ color: 'var(--text-secondary)' }}>
          No database matches
        </span>
      )}
    </>
  )
}
