// How old the evidence was when an investigation ended (sre-02). A run
// concludes on the evidence it collected; when that evidence is older than
// a sample interval (a resumed run, a slow model turn) the panel says so.
// A run resumed on evidence too old to describe the incident ends expired
// with failure code stale_evidence and no diagnosis; the panel says that,
// and offers no re-run control.

import { isStaleExpiry, SAMPLE_INTERVAL_S, STALE_EVIDENCE_TEXT } from './staleEvidence'

function ageText(seconds) {
  if (seconds < 60) return `${seconds} s`
  return `${Math.floor(seconds / 60)} min`
}

const strong = { color: 'var(--text-primary)' }
const muted = { color: 'var(--text-secondary)' }

export function EvidenceAge({ investigation }) {
  const age = Number(investigation?.summary?.evidence_age_s) || 0
  if (isStaleExpiry(investigation)) {
    return (
      <div data-testid="investigation-stale" style={strong}>
        {STALE_EVIDENCE_TEXT}: the newest evidence was {ageText(age)} old when this
        run resumed, so it ended without a diagnosis.
      </div>
    )
  }
  if (investigation?.state !== 'concluded' && investigation?.state !== 'inconclusive') {
    return null
  }
  if (age <= SAMPLE_INTERVAL_S) return null
  return (
    <div data-testid="investigation-evidence-age" style={muted}>
      Concluded on evidence {ageText(age)} old.
    </div>
  )
}
