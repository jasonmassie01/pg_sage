// TOP_CASES_LIMIT is how many ranked cases the Overview shows (cases-11b).
export const TOP_CASES_LIMIT = 5

// topCasesURL asks for the top of the global rank, without evidence.
export function topCasesURL(database) {
  const params = new URLSearchParams({ summary: '1', limit: String(TOP_CASES_LIMIT) })
  if (database && database !== 'all') params.set('database', database)
  return `/api/v1/cases?${params}`
}
