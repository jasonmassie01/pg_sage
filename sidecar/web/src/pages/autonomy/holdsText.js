// earned-06: an earned level of L2 or more holds only while its evidence
// does (the bench report ages out 30 days after it was generated, shadow
// reviews leave their window). The server sends the date it holds until
// (holds_until) and the checks that expire then (expiring_checks); this
// says it in words, with what would renew it.

function renewals(checks) {
  const out = []
  if ((checks || []).includes('bench_fresh')) out.push('a new bench report arrives')
  if ((checks || []).some(c => c.startsWith('shadow_'))) {
    out.push('more packets are reviewed by a person')
  }
  return out.length > 0 ? out : ['new evidence arrives']
}

export function holdsText(level, holdsUntil, checks) {
  if (!holdsUntil) return ''
  const at = new Date(holdsUntil)
  if (Number.isNaN(at.getTime())) return ''
  const day = at.toISOString().slice(0, 10)
  return `${level} holds until ${day} unless ${renewals(checks).join(' or ')}`
}
