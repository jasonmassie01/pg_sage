// Runbook status text and role checks for the runbooks page (playbooks-10).

function latestState(v, status) {
  if (!v.signed_at) return 'draft awaiting signature'
  if (!v.signature_valid) return 'signature invalid, not running'
  return `${status}, not running`
}

// runningStatus says which version runs and what the latest one waits for.
export function runningStatus(rb) {
  const latest = rb.latest_version
  if (rb.status === 'retired') return 'retired: runbook not running'
  const running = rb.running_version || 0
  if (running === 0) {
    const state = rb.latest?.signed_at ? rb.status : 'draft'
    return `v${latest} ${state}: runbook not running`
  }
  if (latest === running) return `v${running} signed and running`
  return `v${running} signed and running; v${latest} ${latestState(rb.latest, rb.status)}`
}

export function canOperate(user) {
  return user?.role === 'admin' || user?.role === 'operator'
}
