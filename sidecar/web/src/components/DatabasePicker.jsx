export function DatabasePicker({ databases, selected, onSelect }) {
  return (
    <select value={selected} onChange={e => onSelect(e.target.value)}
      className="px-3 py-1.5 rounded text-sm"
      style={{
        background: 'var(--bg-hover)',
        color: 'var(--text-primary)',
        border: '1px solid var(--border)',
      }}>
      <option value="all">All Databases</option>
      {databases.map(db => (
        <option key={db.name} value={db.name}>{db.name}</option>
      ))}
    </select>
  )
}
