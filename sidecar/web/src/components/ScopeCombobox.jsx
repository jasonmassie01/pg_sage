import { useId, useMemo, useState } from 'react'
import { useDatabaseSearch } from '../hooks/useDatabaseSearch'
import { parseTagScope, pushRecent, readRecent } from '../lib/fleetScope'

// ScopeCombobox is the header scope picker of a large fleet (ux-10b): an
// ARIA 1.2 combobox whose listbox offers "All databases", a typed tag scope
// ("tag:tier=prod"), recent databases while nothing is typed, and the
// fleet list's matches for the typed text, PICKER_PAGE a page. Closed, it
// names the scope. A failed search keeps the scope and says so in the list.

const control = {
  background: 'var(--bg-hover)',
  color: 'var(--text-primary)',
  border: '1px solid var(--border)',
}
const muted = { color: 'var(--text-secondary)' }

function scopeLabel(selected, selectedTag) {
  if (selectedTag) return `tag:${selectedTag}`
  return !selected || selected === 'all' ? 'All databases' : selected
}

// buildOptions lists the listbox options in order; recent ones carry
// group 'recent'.
function buildOptions({ query, inventory, recent, results, next }) {
  const tag = parseTagScope(query)
  const options = [{ kind: 'all', value: 'all', label: 'All databases' }]
  if (tag) options.push({ kind: 'tag', value: tag, label: `All databases tagged ${tag}` })
  const blank = query.trim() === ''
  const shownRecent = blank ? recent.filter(n => inventory.has(n)) : []
  for (const name of shownRecent) {
    options.push({ kind: 'db', value: name, label: name, group: 'recent' })
  }
  for (const row of results) {
    if (!shownRecent.includes(row.name)) {
      options.push({ kind: 'db', value: row.name, label: row.name })
    }
  }
  if (next) options.push({ kind: 'more', value: next, label: 'Load more' })
  return options
}

// enterTarget is the option Enter picks with nothing highlighted: a typed
// tag scope, the exact name (any case), or the single match.
function enterTarget(options, query) {
  const tag = options.find(o => o.kind === 'tag')
  if (tag) return tag
  const dbs = options.filter(o => o.kind === 'db' && !o.group)
  const low = query.trim().toLowerCase()
  return dbs.find(o => o.value.toLowerCase() === low) || (dbs.length === 1 ? dbs[0] : null)
}

function useComboboxState({ databases, onSelect, onSelectTag }) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [active, setActive] = useState(-1)
  const [recent, setRecent] = useState(readRecent)
  const tag = parseTagScope(query)
  const search = useDatabaseSearch({ open, q: tag ? '' : query.trim(), tag: tag || '' })
  const inventory = useMemo(() => new Set(databases.map(db => db.name)), [databases])
  const options = buildOptions({ query, inventory, recent, results: search.rows,
    next: search.next })

  function close() {
    setOpen(false)
    setQuery('')
    setActive(-1)
  }
  function choose(option) {
    if (!option) return
    if (option.kind === 'more') return search.loadMore()
    close()
    if (option.kind === 'tag') return onSelectTag(option.value)
    if (option.kind === 'db') setRecent(pushRecent(option.value))
    onSelect(option.value)
  }
  function edit(value) {
    setOpen(true)
    setQuery(value)
    setActive(-1)
  }
  return { open, setOpen, query, active, setActive, options, search, close, choose, edit }
}

function onKey(event, box) {
  const { open, options, active, setActive, setOpen, choose, close, query } = box
  if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
    event.preventDefault()
    if (!open) return setOpen(true)
    const step = event.key === 'ArrowDown' ? 1 : -1
    setActive(Math.min(options.length - 1, Math.max(0, active + step)))
  } else if (event.key === 'Enter' && open) {
    event.preventDefault()
    choose(active >= 0 ? options[active] : enterTarget(options, query))
  } else if (event.key === 'Escape') {
    close()
  }
}

export function ScopeCombobox({ databases, selected, selectedTag, onSelect, onSelectTag,
  ...rest }) {
  const base = `scope-${useId().replace(/[^A-Za-z0-9_-]/g, '')}`
  const box = useComboboxState({ databases, onSelect, onSelectTag })
  const optionID = i => `${base}-opt-${i}`
  return (
    <div className="relative">
      <input type="text" role="combobox" aria-label="Database scope"
        aria-expanded={box.open} aria-controls={`${base}-list`} aria-autocomplete="list"
        aria-activedescendant={box.open && box.active >= 0 ? optionID(box.active) : undefined}
        value={box.open ? box.query : scopeLabel(selected, selectedTag)}
        placeholder="Find database or tag:key=value" autoComplete="off" spellCheck={false}
        onFocus={() => box.edit('')} onBlur={box.close}
        onChange={e => box.edit(e.target.value)} onKeyDown={e => onKey(e, box)}
        {...rest} className="px-3 py-1.5 rounded text-sm w-44" style={control} />
      {box.open && <ScopeList id={`${base}-list`} box={box} optionID={optionID} />}
    </div>
  )
}

function ScopeOption({ option, index, box, optionID }) {
  const selected = index === box.active
  return (
    <div id={optionID(index)} role="option" aria-selected={selected}
      onMouseDown={e => e.preventDefault()} onClick={() => box.choose(option)}
      className="px-3 py-1 text-sm cursor-pointer truncate"
      style={{ background: selected ? 'var(--bg-hover)' : 'transparent',
        fontStyle: option.kind === 'more' ? 'italic' : 'normal' }}>
      {option.label}
    </div>
  )
}

function ScopeList({ id, box, optionID }) {
  const { options, search, query } = box
  const indexed = options.map((option, index) => ({ option, index }))
  const recent = indexed.filter(o => o.option.group === 'recent')
  const render = ({ option, index }) => (
    <ScopeOption key={`${option.kind}-${option.value}`} option={option} index={index}
      box={box} optionID={optionID} />
  )
  const shown = search.rows.length
  return (
    <div id={id} role="listbox" aria-label="Database scopes"
      onMouseDown={e => e.preventDefault()}
      className="absolute right-0 z-50 mt-1 w-64 max-h-80 overflow-y-auto rounded border py-1"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      {indexed.filter(o => !o.option.group && o.option.kind !== 'db'
        && o.option.kind !== 'more').map(render)}
      {recent.length > 0 && (
        <div role="group" aria-label="Recent">
          <div className="px-3 pt-1 text-xs uppercase" style={muted}>Recent</div>
          {recent.map(render)}
        </div>
      )}
      {indexed.filter(o => !o.option.group && o.option.kind === 'db').map(render)}
      {search.error && (
        <div role="alert" className="px-3 py-1 text-xs" style={{ color: 'var(--red)' }}>
          Search failed: {search.error.message}. The scope is unchanged.
        </div>
      )}
      {!search.error && !search.loading && shown === 0 && query.trim() !== '' && (
        <div role="status" className="px-3 py-1 text-xs" style={muted}>No database matches</div>
      )}
      {search.total > shown && (
        <div className="px-3 py-1 text-xs" style={muted}>{shown} of {search.total} shown</div>
      )}
      {indexed.filter(o => o.option.kind === 'more').map(render)}
    </div>
  )
}
