// review.js (review-01b): the Review page's wording, links and letter
// colours. A grade the API cannot vouch for is "Unknown: <reason>", never a
// grey letter or an empty cell.

const LETTERS = ['A', 'B', 'C', 'D', 'F']
const MIN_COVERAGE = 60
const MIN_KNOWN = 4
const DIMENSIONS = 8
const REASON_MAX = 100

export const INVALID_GRADE = 'invalid grade'
export const NO_REASON = 'no reason given'

// LETTER_STYLES pair each letter with a background its text reaches 4.5:1
// on (lib/review.test.js checks it), and the letter is always written out,
// so colour is never the only signal.
export const LETTER_STYLES = Object.freeze({
  A: { color: '#ffffff', background: '#166534' },
  B: { color: '#ffffff', background: '#3f6212' },
  C: { color: '#ffffff', background: '#854d0e' },
  D: { color: '#ffffff', background: '#9a3412' },
  F: { color: '#ffffff', background: '#991b1b' },
  unknown: { color: '#ffffff', background: '#374151' },
})

// gradeBadge is what a letter and score show: the letter when both are
// valid (a missing score is allowed), else unknown with the reason.
export function gradeBadge(letter, score, reason) {
  if (letter === null || letter === undefined) {
    return { letter: null, unknown: true, reason: reason || NO_REASON }
  }
  const badScore = score !== undefined && score !== null
    && (typeof score !== 'number' || !Number.isFinite(score) || score < 0 || score > 100)
  if (!LETTERS.includes(letter) || badScore) {
    return { letter: null, unknown: true, reason: INVALID_GRADE }
  }
  return { letter, unknown: false, reason: '' }
}

// dimensionUnknownReason says why a dimension has no letter.
export function dimensionUnknownReason(dim) {
  const checks = dim?.checks || []
  if (checks.length > 0 && checks.every(c => c.status === 'not_applicable')) {
    return 'no check applies on this platform'
  }
  return `coverage ${Number(dim?.coverage || 0)}%, below ${MIN_COVERAGE}%`
}

// overallUnknownReason says why the overall grade has no letter.
export function overallUnknownReason(grade) {
  const known = Number(grade?.known_dimensions || 0)
  return `${known} of ${DIMENSIONS} dimensions known, ${MIN_KNOWN} needed`
}

// shortReason keeps a reason of up to 100 characters whole; a longer one
// is cut to 100 with an ellipsis and its full text goes in title.
export function shortReason(reason) {
  const text = String(reason || '')
  if (text.length <= REASON_MAX) return { text, title: undefined }
  return { text: `${text.slice(0, REASON_MAX - 1)}…`, title: text }
}

// modelPassText is the model pass status line.
export function modelPassText(pass) {
  if (pass?.status === 'done') return `Model pass: done by ${pass.model || 'a model'}`
  const status = pass?.status === 'failed' ? 'failed' : 'skipped'
  return `Model pass: ${status}: ${pass?.reason || NO_REASON}`
}

export const STATUS_LABELS = Object.freeze({
  pass: 'Pass', warn: 'Warn', fail: 'Fail', unknown: 'Unknown',
  not_applicable: 'Not applicable',
})

export function statusLabel(status) {
  return STATUS_LABELS[status] || 'Unknown'
}

const enc = encodeURIComponent

export const gradeURL = db => `/api/v1/databases/${enc(db)}/review/grade`
export const viewedURL = (db, id) => `/api/v1/databases/${enc(db)}/review/runs/${id}/viewed`
export const reviewHref = db => `#/review?db=${enc(db)}`
export const planHref = (plan, db) =>
  `#/actions?tab=plans&plan=${enc(plan)}&database=${enc(db)}`

// reviewParams is the database the page reviews: ?db= first, else the
// picker's database; "all" is no database.
export function reviewParams(route, database) {
  const params = new URLSearchParams(String(route || '').split('?')[1] || '')
  const db = params.get('db') || database
  return db && db !== 'all' ? db : null
}

// contrastRatio is the WCAG 2 contrast ratio of two #rrggbb colours.
export function contrastRatio(fg, bg) {
  const [a, b] = [luminance(fg), luminance(bg)]
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05)
}

function luminance(hex) {
  const n = parseInt(hex.slice(1), 16)
  const [r, g, b] = [n >> 16, (n >> 8) & 255, n & 255].map(v => {
    const c = v / 255
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
  })
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}
