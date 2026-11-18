// agentRequests.js: which external agent requests (the Postgres-specialist
// contract's audit) still wait on a person (ux-11).

// A positive integer queue id (actionFocus.js accepts the same on queue=).
const QUEUE_ID = /^[1-9][0-9]{0,18}$/

// queueOf is the request's approval queue id as a string, or '' when it
// queued none (or the id is not a positive integer).
export function queueOf(rec) {
  const queue = typeof rec?.queue_id === 'number' || typeof rec?.queue_id === 'string'
    ? String(rec.queue_id) : ''
  return QUEUE_ID.test(queue) ? queue : ''
}

// waitingRequests keeps the remediations pg_sage queued for approval whose
// approval is still waiting: its queue id is one of waiting, the change
// approvals the Inbox lists. A decided approval leaves the Inbox, and its
// request then shows only in the ledger.
export function waitingRequests(items, waiting) {
  const open = new Set(Array.isArray(waiting) ? waiting.map(String) : [])
  return (Array.isArray(items) ? items : []).filter(rec => rec?.kind === 'remediation'
    && rec.verdict === 'queued_for_approval' && open.has(queueOf(rec)))
}
