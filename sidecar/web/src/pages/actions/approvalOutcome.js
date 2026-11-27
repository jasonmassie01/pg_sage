// approvalOutcomeMessage turns an approve response into a truthful
// message: executed, approved but not run, or refused (and why). A
// scheduled approval names where it is cancelled: the "Approved, waiting
// for the maintenance window" list inside Waiting on you (ux-05a win 4).
export function approvalOutcomeMessage(status, json) {
  const body = json || {}
  const ok = status >= 200 && status < 300
  if (ok && body.status === 'scheduled') {
    return { ok: true, text: 'Approved. Scheduled for the window opening ' +
      (body.run_after_label || 'of the next maintenance window') +
      '; pg_sage checks the policy again then. Cancel it under Waiting on you > ' +
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

// allowAlwaysOutcomeMessage reads an approve-and-allow response (ux-07a):
// the action's outcome, then whether the class now runs unattended. An
// approval whose promotion was refused says so; it is still approved.
export function allowAlwaysOutcomeMessage(status, json) {
  const body = json || {}
  const action = approvalOutcomeMessage(status, body)
  if (!action.ok) return action
  if (body.allow_always) {
    return { ok: true, text: `${action.text}. It now runs unattended `
      + `(${body.autonomy_level || 'L3'}); pg_sage stops asking.` }
  }
  return { ok: false, text: `${action.text}. `
    + (body.autonomy_error || 'approved; autonomy unchanged') }
}
