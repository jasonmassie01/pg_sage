// whyNot.js: the words and the question of the Why not panel (ux-08b)
// over ux-08a's chain (GET /api/v1/whynot). Every link name, status and
// fix kind the server may send is named here; one this release does not
// know is shown by its sentence alone, never as a pass.

export const LINK_LABELS = {
  observed: 'Observed', finding: 'Finding', candidate: 'Proposed change',
  policy: 'Policy check', window: 'Maintenance window', budget: 'Change budget',
  privilege: 'Privileges', trust: 'Trust', verification_wait: 'Verification wait',
  emergency_stop: 'Emergency stop',
}

export const linkLabel = name => LINK_LABELS[name] || 'Another check'

export const STATUS_LABELS = { pass: 'Pass', fail: 'Fail', unknown: 'Unknown',
  not_applicable: 'Not applicable' }

// statusOf is a link's status as shown: anything this release does not
// know is unknown, so it can never read as a pass.
export const statusOf = link => (STATUS_LABELS[link?.status] ? link.status : 'unknown')

const OBJECT = /^[A-Za-z_][A-Za-z0-9_$]{0,62}\.[A-Za-z_][A-Za-z0-9_$]{0,62}$/

// objectOf is the first schema.name the route accepts, else ''.
export function objectOf(names) {
  return (Array.isArray(names) ? names : []).find(n => OBJECT.test(String(n))) || ''
}

// whyNotURL asks about one finding, or an object and/or a category, or
// the whole database; null without one database to ask about.
export function whyNotURL({ database, findingId, object, category }) {
  if (!database || database === 'all') return null
  const params = new URLSearchParams({ database })
  if (findingId) params.set('finding_id', String(findingId))
  else {
    if (object) params.set('object', object)
    if (category) params.set('category', category)
  }
  return `/api/v1/whynot?${params.toString()}`
}

const linksOf = data => (Array.isArray(data?.links) ? data.links : [])

// firstFailing is the link the server marked first (the emergency stop
// when it fails), else the earliest failing link, else undefined.
export function firstFailing(data) {
  const failing = linksOf(data).filter(l => statusOf(l) === 'fail')
  return failing.find(l => l.first_fail) || failing.find(l => l.name === data?.first_fail)
    || failing[0]
}

// summaryOf is the panel's one-line answer. A chain with an unknown link
// and no failure never says nothing is holding it back.
export function summaryOf(data) {
  const first = firstFailing(data)
  if (first) return `Held back at ${linkLabel(first.name)}: ${first.sentence}`
  const unknown = linksOf(data).filter(l => statusOf(l) === 'unknown').length
  if (unknown > 0) {
    return `Nothing failed, but ${unknown} ${unknown === 1 ? 'check' : 'checks'} could `
      + 'not be made, so it is not known whether this will run.'
  }
  return "Nothing is holding this back; it will run at the executor's next pass."
}

// alsoFailing names the failing links after the first, in chain order.
export function alsoFailing(data) {
  const first = firstFailing(data)
  return linksOf(data).filter(l => statusOf(l) === 'fail' && l !== first)
    .map(l => linkLabel(l.name))
}
