import { useRef, useState } from 'react'
import { usePendingActionsRefetch } from '../../components/Layout'
import { useToast } from '../../components/Toast'
import { approvalOutcomeMessage } from './ApprovalCard'
import { actionRowKey } from './actionFormat'

function decisionURL(action, verb) {
  const dbParam = action?.database_name
    ? `?database=${encodeURIComponent(action.database_name)}` : ''
  return `/api/v1/actions/${action.id}/${verb}${dbParam}`
}

function postDecision(action, verb, body) {
  return fetch(decisionURL(action, verb), {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    ...(body ? { body: JSON.stringify(body) } : {}),
  })
}

// usePendingActions holds the table view's approve and reject decisions.
// A decision already in flight for a row is not sent again, so a double
// click posts one request (dashboard-31).
export function usePendingActions(refetch) {
  const [rejectId, setRejectId] = useState(null)
  const [rejectReason, setRejectReason] = useState('')
  const [actionMsg, setActionMsg] = useState(null)
  const inFlight = useRef(new Set())
  const refetchPendingCount = usePendingActionsRefetch()
  const toast = useToast()
  const report = (ok, text) => {
    if (ok) toast.success(text)
    else toast.error(text)
    setActionMsg({ type: ok ? 'success' : 'error', text })
  }
  const once = async (action, verb, run) => {
    const key = `${verb}:${actionRowKey(action)}`
    if (inFlight.current.has(key)) return
    inFlight.current.add(key)
    try {
      await run()
    } finally {
      inFlight.current.delete(key)
    }
  }
  const approve = action => once(action, 'approve',
    () => approveAction(action, { setActionMsg, report, toast, refetch, refetchPendingCount }))
  const reject = action => once(action, 'reject', () => rejectAction(action, rejectReason, {
    setActionMsg, report, toast, refetch, refetchPendingCount, setRejectId, setRejectReason }))
  const toggleReject = row =>
    setRejectId(rejectId === actionRowKey(row) ? null : actionRowKey(row))
  return { rejectId, toggleReject, rejectReason, setRejectReason, actionMsg, approve, reject }
}

async function approveAction(action, ctx) {
  ctx.setActionMsg(null)
  try {
    const res = await postDecision(action, 'approve')
    const json = await res.json()
    const result = approvalOutcomeMessage(res.status, json)
    ctx.report(result.ok, result.text)
    ctx.refetch()
    ctx.refetchPendingCount()
  } catch (err) {
    ctx.toast.error(`Approve failed: ${err.message}`)
    ctx.setActionMsg({ type: 'error', text: err.message })
  }
}

async function rejectAction(action, rejectReason, ctx) {
  ctx.setActionMsg(null)
  const reason = rejectReason.trim()
  if (!reason) {
    ctx.setActionMsg({ type: 'error', text: 'A rejection reason is required' })
    return
  }
  try {
    const res = await postDecision(action, 'reject', { reason })
    const json = await res.json()
    if (json.ok) ctx.report(true, `Action ${action.id} rejected`)
    else ctx.report(false, json.error || 'Reject failed')
    ctx.setRejectId(null)
    ctx.setRejectReason('')
    ctx.refetch()
    ctx.refetchPendingCount()
  } catch (err) {
    ctx.toast.error(`Reject failed: ${err.message}`)
    ctx.setActionMsg({ type: 'error', text: err.message })
  }
}
