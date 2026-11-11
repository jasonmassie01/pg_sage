import { caseID } from './casePresentation'

// CASE_IDS_PER_REQUEST matches the API's cap on case_id values.
export const CASE_IDS_PER_REQUEST = 100

// caseChunks splits the visible cases into runs whose distinct case ids
// fit one investigations request, keeping their order.
export function caseChunks(cases) {
  const chunks = []
  let current = null
  for (const row of cases) {
    const id = caseID(row)
    if (!current || (!current.ids.has(id) && current.ids.size >= CASE_IDS_PER_REQUEST)) {
      current = { cases: [], ids: new Set() }
      chunks.push(current)
    }
    current.cases.push(row)
    current.ids.add(id)
  }
  return chunks.map(c => ({ cases: c.cases, ids: [...c.ids] }))
}

// investigationsURL asks for the newest investigation of each case id in
// the selected database(s): two or more ids are one latest-per-case page;
// a single id is that case's history cut to its newest (limit=1).
export function investigationsURL(database, ids) {
  const params = new URLSearchParams({ database: database || 'all' })
  for (const id of ids) params.append('case_id', id)
  if (ids.length === 1) params.set('limit', '1')
  return `/api/v1/investigations?${params.toString()}`
}

// investigationsByCase keeps each case's newest investigation (the API
// lists newest first), keyed by database and case id ("|" never appears
// in a database name), so a same-named case of another database never
// borrows it.
export function investigationsByCase(items) {
  const out = new Map()
  for (const inv of items || []) {
    const key = `${inv.database}|${inv.case_id}`
    if (!out.has(key)) out.set(key, inv)
  }
  return out
}
