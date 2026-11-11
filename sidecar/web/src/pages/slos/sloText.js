// Text helpers of the SLOs page (playbooks-09): reasons, durations and
// the error-budget cell. An unknown budget is never shown as a number of
// budget left, and never as the bare word "unknown".

export function humanize(reason) {
  return String(reason || '').replaceAll('_', ' ')
}

// formatDuration is a span of milliseconds as its two largest units
// ("40m", "2h 5m", "3d 2h"), or null for a missing or negative span.
export function formatDuration(ms) {
  if (!Number.isFinite(ms) || ms < 0) return null
  const sec = Math.floor(ms / 1000)
  if (sec < 60) return `${sec}s`
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min}m`
  const hours = Math.floor(min / 60)
  if (hours < 24) return min % 60 ? `${hours}h ${min % 60}m` : `${hours}h`
  const days = Math.floor(hours / 24)
  return hours % 24 ? `${days}d ${hours % 24}h` : `${days}d`
}

// since is the time from an RFC 3339 timestamp to now, formatted, or null.
export function since(timestamp, now = Date.now()) {
  if (!timestamp) return null
  return formatDuration(now - new Date(timestamp).getTime())
}

// Shares are floored: a window 89.9% covered (partial) never reads 90%.
function floorPercent(share) {
  return Math.floor(share * 100 + 1e-9)
}

// budgetText is the budget cell as lines: the budget left when it is
// known, else why it is unknown; a partial window adds the share covered
// and, when the server measured it, what the covered part spent.
export function budgetText(slo) {
  const left = slo.budget_remaining
  if (left !== null && left !== undefined) return [`${Math.round(left * 100)}%`]
  const reason = slo.budget_unknown
  if (!reason) return ['unknown (no reason recorded)']
  if (reason !== 'partial_window' || typeof slo.budget_coverage !== 'number') {
    return [humanize(reason)]
  }
  const lines = [`partial window: ${floorPercent(slo.budget_coverage)}% covered`]
  if (typeof slo.budget_spent_so_far === 'number') {
    lines.push(`spent so far: ${Math.round(slo.budget_spent_so_far * 100)}%`)
  }
  return lines
}
