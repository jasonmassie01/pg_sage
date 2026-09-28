// Live create/destroy use the server-owned authority contract (G8-B13):
// authorize-live issues the plan, estimate and single-use authorization;
// the operator confirms the estimate; execute/destroy-live then carries the
// exact tuple. The browser never invents cost estimates or approvals.

function liveOperation(action) {
  return action === 'destroy-live' ? 'destroy' : 'create'
}

function liveEndpoint(action) {
  return action === 'destroy-live' ? 'destroy-live' : 'execute'
}

export function liveConfirmMessage(id, action, authorization) {
  const estimate = authorization.estimate || {}
  const plan = authorization.plan || {}
  const verb = action === 'destroy-live' ? 'Live destroy' : 'Live create'
  const cost = Number(estimate.estimated_cost_usd || 0).toFixed(2)
  return [
    `${verb} cloud resource for ${id}?`,
    `Provider ${plan.provider || 'unknown'} in ${plan.region || 'unknown region'}.`,
    `Estimated lease cost $${cost} (${estimate.confidence || 'unknown'} confidence).`,
  ].join('\n')
}

export async function runLiveProvisionAction(postJSON, id, action, confirmFn) {
  const operation = liveOperation(action)
  const idempotencyKey = `ui-${operation}-${id}-${Date.now().toString(36)}`
  const authorization = await postJSON(
    `/api/v1/agent-dbs/${id}/provision/authorize-live`,
    { operation, idempotency_key: idempotencyKey },
    { 'Idempotency-Key': idempotencyKey },
  )
  if (confirmFn && !confirmFn(liveConfirmMessage(id, action, authorization))) {
    return null
  }
  return postJSON(
    `/api/v1/agent-dbs/${id}/provision/${liveEndpoint(action)}`,
    {
      plan_hash: authorization.plan_hash,
      estimate_id: authorization.estimate_id,
      authorization_id: authorization.authorization_id,
      idempotency_key: authorization.idempotency_key,
    },
    { 'Idempotency-Key': authorization.idempotency_key },
  )
}

// Restore verification is an admin attestation of a real drill with
// evidence (G8-B11); the generic backup endpoint cannot grant it.
export async function attestRestoreDrill(postJSON, id, evidenceURI) {
  return postJSON(`/api/v1/agent-dbs/${id}/backups/restore-drill`, {
    backup_id: `restore_drill_${Date.now().toString(36)}`,
    evidence_uri: evidenceURI,
    target: 'operator restore drill',
    checks: ['operator verified restored data'],
  })
}

// Deployment listing is paginated server-side (G8-B25).
export async function fetchDeploymentPage(cursor) {
  const res = await fetch(
    `/api/v1/agent-dbs?cursor=${encodeURIComponent(cursor)}`,
    { credentials: 'include' },
  )
  const data = await res.json().catch(() => ({}))
  if (!res.ok) {
    throw new Error(data.error || `Request failed: ${res.status}`)
  }
  return data
}

export function mergeDeployments(base, extra) {
  const seen = new Set()
  const out = []
  for (const dep of [...base, ...extra]) {
    if (!seen.has(dep.deployment_id)) {
      seen.add(dep.deployment_id)
      out.push(dep)
    }
  }
  return out
}
