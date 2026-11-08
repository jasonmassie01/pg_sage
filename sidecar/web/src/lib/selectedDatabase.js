// A singleton fleet always has a useful concrete scope, including on a first
// visit or after a stored database is removed. Multi-database fleets retain
// the explicit all-databases scope. Keep stored scope while inventory loads.
export function resolveSelectedDB(selected, databases) {
  if (!Array.isArray(databases)) return selected || 'all'
  const names = [...new Set(databases.map(db => db?.name).filter(Boolean))]
  if (selected !== 'all' && names.includes(selected)) return selected
  return names.length === 1 ? names[0] : 'all'
}
