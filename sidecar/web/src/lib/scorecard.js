// scorecard.js (review-05b): the wording of Sage Review's scorecard and
// model gate (GET /api/v1/databases/{db}/review/scorecard, review-05a). The
// numbers are the server's; this only shows them. A count that cannot be
// one (negative, fractional, or more than its denominator) reads "invalid",
// never a number, and a rate over fewer than 10 reads "too few to judge".

export const MIN_DENOMINATOR = 10
export const INVALID = 'invalid'
export const TOO_FEW = 'too few to judge'
export const NO_REVIEWS = 'No reviews yet'

const ORIGINS = ['generator', 'model', 'person']
const ORIGIN_LABELS = Object.freeze({
  generator: 'Generators', model: 'Model', person: 'People',
})

// eligible is a stored state the gate files at once (review-05a), so a row
// in it is shown by name rather than as an unknown verdict.
export const GATE_VERDICTS = Object.freeze({
  not_eligible: 'Not eligible',
  eligible: 'Eligible, not yet filed for approval',
  pending_approval: "Waiting for an admin's approval",
  granted: 'Granted by an admin',
  revoked: 'Revoked',
  model_on: 'Model pass stays on',
  model_off: 'Model pass is off for this model',
})

const enc = encodeURIComponent

// scorecardDatabase is the database whose scorecard a page shows: the
// whole fleet ("all") has none.
export const scorecardDatabase = database =>
  (database && database !== 'all' ? database : null)

export const scorecardURL = db => `/api/v1/databases/${enc(db)}/review/scorecard`
// The G-M1 promotion is decided in the Inbox, like every autonomy promotion.
export const INBOX_HREF = '#/'

export const isCount = v => Number.isInteger(v) && v >= 0

export function countText(v) {
  return isCount(v) ? String(v) : INVALID
}

// ratio is k of n and its rate: invalid when either is not a count or k is
// more than n (a rate above 100%), too few to judge when n is under 10.
export function ratio(k, n) {
  if (!isCount(k) || !isCount(n) || k > n) {
    return { text: INVALID, rate: INVALID, judged: false, valid: false }
  }
  const text = `${k} of ${n}`
  if (n < MIN_DENOMINATOR) return { text, rate: TOO_FEW, judged: false, valid: true }
  return { text, rate: `${Math.round((100 * k) / n)}%`, judged: true, valid: true }
}

// originRows are the scorecard's origins, generators, model and people
// first, then any other origin by name.
export function originRows(byOrigin) {
  const source = byOrigin && typeof byOrigin === 'object' ? byOrigin : {}
  const keys = Object.keys(source)
  const known = ORIGINS.filter(o => keys.includes(o))
  const other = keys.filter(k => !ORIGINS.includes(k)).sort()
  return [...known, ...other].map(origin => ({
    origin, label: ORIGIN_LABELS[origin] || origin, counts: source[origin] || {},
  }))
}

// gateRow is the newest evaluation of rule in gate (the API sends the
// newest row of each rule), or null.
export function gateRow(gate, rule) {
  if (!Array.isArray(gate)) return null
  return gate.find(row => row?.rule === rule) || null
}

export function verdictLabel(verdict) {
  return GATE_VERDICTS[verdict] || `Unknown verdict (${String(verdict || 'none')})`
}

function rateClause(name, pair, suffix = '') {
  const r = ratio(pair?.k, pair?.n)
  return r.judged ? `${name} ${r.rate}${suffix}` : `${name}${suffix}: ${r.rate}`
}

const hasPair = p => p && typeof p === 'object' && 'k' in p && 'n' in p

// gateLine is the model gate's line: the reviews counted against the
// minimum, the model's improved rate against the generators' and the
// model's revert rate. Without an evaluation it is 0 of the minimum; an
// evaluation without its counts shows its reason instead.
export function gateLine(row, thresholds) {
  const min = thresholds?.min_reviews
  if (!row) {
    return `Model gate: 0 of ${isCount(min) && min > 0 ? min : INVALID} reviews; `
      + 'not evaluated yet'
  }
  const ev = row.evidence
  const pairs = ['reviews', 'model_improved', 'generator_improved', 'model_reverts']
  if (!ev || !pairs.every(p => hasPair(ev[p]))) {
    return `Model gate: ${row.reason || 'no reason given'}`
  }
  return `Model gate: ${ratio(ev.reviews.k, ev.reviews.n).text} reviews; `
    + `${rateClause('model', ev.model_improved, ' improved')} vs `
    + `${rateClause('generators', ev.generator_improved)}; `
    + `${rateClause('reverts', ev.model_reverts)}`
}

// reviewsCount is the scorecard's review count, 0 without one.
export function reviewsCount(scorecard) {
  const n = scorecard?.reviews
  if (n === undefined || n === null) return 0
  return isCount(n) ? n : null
}
