// A run resumed on evidence too old to describe the incident ends expired
// with failure code stale_evidence and no diagnosis (sre-02).

export const STALE_EVIDENCE_TEXT = 'Evidence too old to conclude'

export function isStaleExpiry(investigation) {
  return investigation?.state === 'expired' &&
    investigation?.failure_code === 'stale_evidence'
}
