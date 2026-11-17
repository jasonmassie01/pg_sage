// Phase 1.3 verification verdicts as the UI names them. Only "Improved"
// earns trust; neutral, insufficient evidence and unverifiable are
// neither credit nor harm; a regression is rolled back. lab-01c names a
// neutral verdict "No change" and insufficient evidence "Inconclusive".
export const VERDICTS = {
  improved: { label: 'Improved', color: 'var(--green)' },
  neutral: { label: 'No change', color: 'var(--text-secondary)' },
  regressed: { label: 'Regressed', color: 'var(--red)' },
  insufficient_evidence: { label: 'Inconclusive', color: '#b58900' },
  unverifiable: { label: 'Unverifiable', color: '#b58900' },
  pending: { label: 'Verifying', color: 'var(--blue, #3b82f6)' },
}

export function verdictLabel(verdict) {
  return (VERDICTS[verdict] || { label: verdict || 'Unknown' }).label
}
