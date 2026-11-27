// Risk tiers as the API reports them (safe, moderate, high or high_risk, ...).
// HIGH is the tier that always needs an operator's explicit approval.
const HIGH_TIERS = new Set(['high', 'high_risk'])

export function isHighRisk(tier) {
  return HIGH_TIERS.has(tier)
}

export function riskLabel(value) {
  return ({ safe: 'Low', moderate: 'Moderate', high: 'High', high_risk: 'High', unknown: 'Unknown',
    none: 'Advisory only', read_only: 'Read only' })[value]
    || (value ? value.replaceAll('_', ' ') : 'Unknown')
}
