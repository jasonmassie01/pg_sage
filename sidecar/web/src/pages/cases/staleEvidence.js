// A run resumed on evidence too old to describe the incident ends expired
// with failure code stale_evidence and no diagnosis (sre-02).

// SAMPLE_INTERVAL_S is the longest configurable sample interval (sre
// MaxSampleInterval): evidence no older than it was collected during the
// run itself, so the panel states no evidence age.
export const SAMPLE_INTERVAL_S = 30

export const STALE_EVIDENCE_TEXT = 'Evidence too old to conclude'

export function isStaleExpiry(investigation) {
  return investigation?.state === 'expired' &&
    investigation?.failure_code === 'stale_evidence'
}
