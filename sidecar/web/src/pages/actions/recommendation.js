// revisionPin names the recommendation revision a queued proposal pins
// (its approval approves exactly that revision).
export function revisionPin(row) {
  if (!row?.recommendation_id) return ''
  return `#${row.recommendation_id} r${row.recommendation_revision}`
}
