/* eslint-disable react-refresh/only-export-components */

// LadderSection shows what a queued change was tested on before
// production (lab-03a): one line per verification-ladder record, in the
// order recorded, or the server's note when nothing was recorded. A
// ladder that is not an array (unreadable) renders nothing. Every value
// renders as React text.

const TIERS = {
  1: 'Tested on plans only', 2: 'Tested on a clone', 3: 'Tested in production',
  4: 'Tested on the fleet',
}
const TIER_NAMES = {
  plan_only: TIERS[1], clone: TIERS[2], canary: TIERS[3], fleet: TIERS[4],
}
const METRICS = { plan_cost: 'planned cost', blocks_per_call: 'blocks per call' }

function isNumber(v) {
  return typeof v === 'number' && Number.isFinite(v)
}

function signedPct(v) {
  const rounded = Math.round(v * 10) / 10
  return `${rounded > 0 ? '+' : ''}${rounded}%`
}

function reasonOf(record) {
  const skipped = Array.isArray(record.skipped) ? record.skipped : []
  const own = skipped.find(s => s && s.tier === record.tier) || skipped[0]
  if (own && own.reason) return String(own.reason)
  const ev = record.evidence
  return ev && ev.reason ? String(ev.reason) : ''
}

export function ladderLine(record) {
  if (!record || typeof record !== 'object') return ''
  const tier = TIERS[record.tier] || `Tier ${record.tier}`
  const reason = reasonOf(record)
  if (record.result === 'skipped') return `Skipped: ${reason || 'not tested'}`
  if (record.result !== 'passed') {
    return `${tier}: ${record.result}${reason ? ` (${reason})` : ''}`
  }
  let text = tier
  if (isNumber(record.predicted)) {
    const metric = METRICS[record.metric] || 'change'
    text += `: ${metric} ${signedPct(record.predicted)}`
  } else {
    text += ': passed'
  }
  const receipt = record.evidence && record.evidence.receipt_id
  if (receipt) text += ` (receipt ${receipt})`
  return text
}

export function ledgerTierLabel(tier) {
  return TIER_NAMES[tier] || ''
}

export function LadderSection({ ladder, note }) {
  const records = Array.isArray(ladder) ? ladder : null
  if (!records?.length && !note) return null
  return (
    <div data-testid="approval-ladder">
      <div className="text-xs font-medium mb-1" style={{ color: 'var(--text-secondary)' }}>
        Tested before production
      </div>
      {records?.length ? (
        <ul className="text-sm list-disc ml-4" style={{ color: 'var(--text-primary)' }}>
          {records.map((r, i) => <li key={i}>{ladderLine(r)}</li>)}
        </ul>
      ) : (
        <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>{String(note)}</p>
      )}
    </div>
  )
}
