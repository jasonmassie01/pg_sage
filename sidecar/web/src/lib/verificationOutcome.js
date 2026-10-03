// Phase 1.3 verification verdicts as the UI names them. Only "Improved"
// earns trust; neutral, insufficient evidence and unverifiable are
// neither credit nor harm; a regression is rolled back.
export const VERDICTS = {
  improved: { label: 'Improved', color: 'var(--green)' },
  neutral: { label: 'Neutral', color: 'var(--text-secondary)' },
  regressed: { label: 'Regressed', color: 'var(--red)' },
  insufficient_evidence: { label: 'Insufficient evidence', color: '#b58900' },
  unverifiable: { label: 'Unverifiable', color: '#b58900' },
  pending: { label: 'Verifying', color: 'var(--blue, #3b82f6)' },
}

export function verdictLabel(verdict) {
  return (VERDICTS[verdict] || { label: verdict || 'Unknown' }).label
}
