import { FLEET_SUMMARY_FROM } from '../lib/fleetScope'
import { ScopeCombobox } from './ScopeCombobox'

// PICKER_FILTER_FROM is the fleet size from which the scope picker is the
// searched combobox (ux-10b); a smaller fleet keeps the plain select.
export const PICKER_FILTER_FROM = FLEET_SUMMARY_FROM

const control = {
  background: 'var(--bg-hover)',
  color: 'var(--text-primary)',
  border: '1px solid var(--border)',
}

// DatabasePicker is the header scope control: nothing before a database
// exists, a plain select for a small fleet (one database included, as
// ask_route.test.jsx pins), and from PICKER_FILTER_FROM up a combobox
// served by the fleet list's search.
export function DatabasePicker({ databases, selected, selectedTag = '', onSelect,
  onSelectTag = () => {}, ...rest }) {
  if (!Array.isArray(databases) || databases.length === 0) return null
  if (databases.length >= PICKER_FILTER_FROM) {
    return <ScopeCombobox databases={databases} selected={selected}
      selectedTag={selectedTag} onSelect={onSelect} onSelectTag={onSelectTag} {...rest} />
  }
  const tagValue = `tag:${selectedTag}`
  return (
    <select aria-label="Database scope" value={selectedTag ? tagValue : selected}
      onChange={e => {
        if (e.target.value !== tagValue) onSelect(e.target.value)
      }}
      {...rest}
      className="px-3 py-1.5 rounded text-sm" style={control}>
      <option value="all">All Databases</option>
      {selectedTag && <option value={tagValue}>{tagValue}</option>}
      {databases.map(db => (
        <option key={db.name} value={db.name}>{db.name}</option>
      ))}
    </select>
  )
}
