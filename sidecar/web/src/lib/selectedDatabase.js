// resolveSelectedDB returns the effective database selection. A name
// restored from localStorage that no longer exists in the fleet falls
// back to 'all' so the UI is never stuck on a deleted database with
// the picker hidden (G9-B09). While the fleet is loading the stored
// value is kept.
export function resolveSelectedDB(selected, databases) {
  if (!selected || selected === 'all') return 'all'
  if (!Array.isArray(databases)) return selected
  return databases.some(db => db?.name === selected) ? selected : 'all'
}
