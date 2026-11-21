// trustLevel.js: the words for a database's trust level, as the fleet
// summary reports it (a number or a name).

const TRUST_LEVEL_LABELS = {
  0: 'Observation',
  1: 'Advisory',
  2: 'Autonomous',
  observation: 'Observation',
  advisory: 'Advisory',
  autonomous: 'Autonomous',
}

// formatTrustLevel names a trust level; null for none, and an unknown
// level as it came.
export function formatTrustLevel(raw) {
  if (raw === null || raw === undefined || raw === '') return null
  const key = typeof raw === 'number' ? raw : String(raw).toLowerCase()
  return TRUST_LEVEL_LABELS[key] || String(raw)
}
