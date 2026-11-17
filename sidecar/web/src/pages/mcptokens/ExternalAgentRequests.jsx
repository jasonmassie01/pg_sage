// The Postgres-specialist contract (roadmap phase 3): which external agent
// (each one a named MCP token) asked pg_sage what, on which database, and
// what pg_sage's gate answered. The list never shows the caller's free text.
import { useEffect, useState } from 'react'
import { formatTime } from '../../lib/time'
import { investigationID } from '../cases/caseFocus'

export const REQUESTS_URL = '/api/v1/specialist-requests'

const card = { background: 'var(--bg-card)', border: '1px solid var(--border)' }
const errorStyle = {
  background: 'rgba(239,68,68,0.1)',
  color: '#ef4444',
  border: '1px solid rgba(239,68,68,0.3)',
}

async function loadRequests() {
  const res = await fetch(`${REQUESTS_URL}?limit=50`, { credentials: 'include' })
  let body = null
  try {
    body = await res.json()
  } catch (err) {
    if (!(err instanceof SyntaxError)) throw err
  }
  if (!res.ok) {
    const msg = typeof body?.error === 'string' ? body.error : `status ${res.status}`
    throw new Error(msg)
  }
  return Array.isArray(body?.items) ? body.items : []
}

const outboundText = {
  pending: 'note pending', delivered: 'note delivered', failed: 'note failed',
}

function what(rec) {
  if (rec.kind === 'remediation') {
    return `remediation ${(rec.verdict || '').replaceAll('_', ' ')}`.trim()
  }
  return rec.created ? 'opened' : `attached (${rec.match || 'existing'})`
}

// A positive integer queue id (actionFocus.js accepts the same on queue=).
const QUEUE_ID = /^[1-9][0-9]{0,18}$/

function hashLink(path, params) {
  return `#/${path}?${new URLSearchParams(params)}`
}

// InvestigationCell links a canonical investigation id to its case on
// Cases (the investigation= focus); any other id is shown, never linked.
function InvestigationCell({ rec }) {
  const raw = typeof rec.investigation_id === 'string' ? rec.investigation_id : ''
  const id = investigationID(raw)
  if (!id) return <code>{raw.slice(0, 8)}</code>
  const params = { investigation: id, ...(rec.database ? { database: rec.database } : {}) }
  return (
    <a href={hashLink('cases', params)} title={id} style={{ color: 'var(--accent)' }}
      data-testid={`specialist-request-investigation-${rec.id}`}>
      <code>{id.slice(0, 8)}</code>
    </a>
  )
}

// ApprovalCell links the approval a remediation queued to Needs your
// review (ux-01b's tab= and queue=); nothing when it queued none.
function ApprovalCell({ rec }) {
  const queue = typeof rec.queue_id === 'number' || typeof rec.queue_id === 'string'
    ? String(rec.queue_id) : ''
  if (!QUEUE_ID.test(queue)) return null
  const params = { tab: 'pending', queue, ...(rec.database ? { database: rec.database } : {}) }
  return (
    <a href={hashLink('actions', params)} style={{ color: 'var(--accent)' }}
      data-testid={`specialist-request-approval-${rec.id}`}>
      Approval #{queue}
    </a>
  )
}

function RequestRow({ rec }) {
  const ref = rec.external_ref
  return (
    <tr data-testid={`specialist-request-${rec.id}`}
      style={{ borderTop: '1px solid var(--border)' }}>
      <td className="py-1 pr-3">{formatTime(rec.created_at)}</td>
      <td className="py-1 pr-3" title={rec.actor}>{rec.identity_name || rec.actor}</td>
      <td className="py-1 pr-3">{rec.kind}</td>
      <td className="py-1 pr-3">{rec.database}</td>
      <td className="py-1 pr-3"><InvestigationCell rec={rec} /></td>
      <td className="py-1 pr-3">{what(rec)}</td>
      <td className="py-1 pr-3 whitespace-nowrap"><ApprovalCell rec={rec} /></td>
      <td className="py-1 pr-3">{ref ? `${ref.system} ${ref.id}` : ''}</td>
      <td className="py-1">{outboundText[rec.outbound] || ''}</td>
    </tr>
  )
}

export function ExternalAgentRequests() {
  const [state, setState] = useState({ items: [], loaded: false, error: null })
  useEffect(() => {
    let live = true
    loadRequests()
      .then(items => { if (live) setState({ items, loaded: true, error: null }) })
      .catch(err => { if (live) setState({ items: [], loaded: true, error: err.message }) })
    return () => { live = false }
  }, [])
  return (
    <div className="rounded-lg p-4 mt-6 text-sm" style={card}
      data-testid="specialist-requests">
      <h2 className="text-sm font-semibold mb-1" style={{ color: 'var(--text-primary)' }}>
        External agent requests
      </h2>
      <p className="mb-3" style={{ color: 'var(--text-secondary)' }}>
        Other agents (PagerDuty, Datadog, AWS DevOps Agent) call pg_sage&apos;s
        Postgres-specialist contract with their MCP token. They can open and read
        investigations and request a remediation; pg_sage&apos;s gate decides.
      </p>
      {state.error && (
        <div data-testid="specialist-requests-error" role="alert"
          className="p-3 rounded" style={errorStyle}>
          Could not load the request audit: {state.error}
        </div>
      )}
      {state.loaded && !state.error && state.items.length === 0 && (
        <div data-testid="specialist-requests-empty"
          style={{ color: 'var(--text-secondary)' }}>
          No external agent has called the contract yet.
        </div>
      )}
      {state.items.length > 0 && (
        <div className="overflow-x-auto" data-testid="specialist-requests-scroll">
          <table className="w-full text-left">
            <thead>
              <tr style={{ color: 'var(--text-secondary)' }}>
                <th>When</th><th>Agent</th><th>Kind</th><th>Database</th>
                <th>Investigation</th><th>What</th><th>Approval</th><th>Reference</th>
                <th>Result post</th>
              </tr>
            </thead>
            <tbody>
              {state.items.map(rec => <RequestRow key={rec.id} rec={rec} />)}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
