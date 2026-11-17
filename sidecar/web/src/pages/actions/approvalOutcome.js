// approvalOutcomeMessage turns an approve response into a truthful
// message: executed, approved but not run, or refused (and why). A
// scheduled approval names where it is cancelled: the "Approved, waiting
// for the maintenance window" list inside Needs your review (ux-05a win 4).
export function approvalOutcomeMessage(status, json) {
  const body = json || {}
  const ok = status >= 200 && status < 300
  if (ok && body.status === 'scheduled') {
    return { ok: true, text: 'Approved. Scheduled for the window opening ' +
      (body.run_after_label || 'of the next maintenance window') +
      '; pg_sage checks the policy again then. Cancel it under Needs your review > ' +
      'Approved, waiting for the maintenance window.' }
  }
  if (ok && body.executed) {
    return { ok: true, text: 'Approved and executed; verification: ' +
      (body.verification_status || 'pending') }
  }
  if (ok && (body.execution_started === true || body.status === 'executing')) {
    return { ok: true, text: 'Approval accepted; execution started. '
      + 'Completion and outcome verification are still pending.' }
  }
  if (ok && body.ok === true && body.executed == null) {
    return { ok: true, text: 'Approval accepted. Execution has not been confirmed; '
      + 'check In progress and History for the recorded result.' }
  }
  if (ok) {
    return { ok: false, text: 'The action did not run: ' +
      (body.error || 'execution was not confirmed') }
  }
  if (status === 409 && body.code === 'content_changed') {
    return { ok: false, text: 'This action changed after this card was shown. ' +
      'Reload and review it again.' }
  }
  return { ok: false, text: body.error || `Approve failed (${status})` }
}
