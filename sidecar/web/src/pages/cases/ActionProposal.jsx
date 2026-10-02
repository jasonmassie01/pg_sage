import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { useToast } from '../../components/Toast'

// Sage SRE actions of an investigation (AI-SRE-SPEC §9): the proposed
// mitigation with its exact target, cited evidence, repair contract and
// policy verdict, then approval, denial and the recovery result. Approval
// and denial use the existing approval API (attribution, single use).
// Viewers see everything and get no controls.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const boxStyle = { borderColor: 'var(--border)' }
const buttonStyle = { ...strong, border: '1px solid var(--border)' }

const STATE_LABELS = {
  proposed: 'Proposed',
  executing: 'Executing',
  executed: 'Executed',
  denied: 'Denied',
  expired: 'Expired',
  refused: 'Refused before signalling',
  failed: 'Failed',
  uncertain: 'Outcome uncertain',
}

function canOperate(user) {
  return user?.role === 'admin' || user?.role === 'operator'
}

function proposalsPath(database, investigationId) {
  return `/api/v1/databases/${encodeURIComponent(database)}` +
    `/investigations/${encodeURIComponent(investigationId)}/proposals`
}

function stateLabel(p) {
  if (p.state === 'requested') {
    return p.approval?.status === 'pending' ? 'Awaiting approval' :
      `Approval ${p.approval?.status || 'requested'}`
  }
  return STATE_LABELS[p.state] || p.state
}

async function post(url, body) {
  const res = await fetch(url, {
    method: 'POST', credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || `Request failed (${res.status})`)
  return data
}

export function ActionProposals({ database, investigationId, user }) {
  const path = proposalsPath(database, investigationId)
  const { data, refetch } = useAPI(path, 15000)
  const items = data?.items || []
  if (items.length === 0) return null
  return (
    <div data-testid="action-proposals" className="space-y-2">
      <div className="font-medium" style={strong}>Proposed action</div>
      {items.map(p => (
        <ProposalCard key={p.id} proposal={p} database={database} path={path}
          operator={canOperate(user)} onDone={refetch} />
      ))}
    </div>
  )
}

function ProposalCard({ proposal: p, database, path, operator, onDone }) {
  if (p.state === 'ineligible') {
    return (
      <div data-testid={`action-proposal-${p.id}`} className="rounded border p-2"
        style={boxStyle}>
        <div style={strong}>Not proposed ({p.reason})</div>
        <div style={muted}>{p.detail}</div>
      </div>
    )
  }
  return (
    <div data-testid={`action-proposal-${p.id}`} className="rounded border p-2 space-y-1"
      style={boxStyle}>
      <div className="flex flex-wrap gap-2">
        <span className="font-medium" style={strong}>{stateLabel(p)}</span>
        <code style={strong}>{p.sql}</code>
      </div>
      <Target target={p.target} evidenceIDs={p.evidence_ids || []} />
      <Contract contract={p.contract || {}} />
      <Policy policy={p.policy || {}} />
      {p.reason && <div style={muted}>Reason: {p.reason} {p.detail}</div>}
      <Recovery recovery={p.recovery} />
      {operator && (
        <Controls proposal={p} database={database} path={path} onDone={onDone} />
      )}
    </div>
  )
}

function Target({ target, evidenceIDs }) {
  if (!target) return null
  return (
    <div style={muted}>
      Target: pid {target.pid}, user {target.user}, database {target.database},
      backend started {new Date(target.backend_start).toLocaleString()}, blocking{' '}
      {target.blocking}; observed {new Date(target.observed_at).toLocaleString()}.
      {' '}Evidence:{' '}
      {evidenceIDs.map(id => (
        <a key={id} href={`#evidence-${id}`} data-testid={`evidence-link-${id}`}
          className="underline mr-1" style={{ color: 'var(--accent)' }}>
          evidence
        </a>
      ))}
    </div>
  )
}

function Contract({ contract }) {
  return (
    <div style={muted}>
      <div>
        Repair contract: {contract.reversibility}; scope {contract.scope}
      </div>
      <ul className="list-disc pl-4">
        {(contract.preconditions || []).map(c => <li key={`pre-${c}`}>Requires: {c}</li>)}
        {(contract.post_conditions || []).map(c => <li key={`post-${c}`}>Verifies: {c}</li>)}
        {(contract.never_do || []).map(c => <li key={`never-${c}`}>{c}</li>)}
      </ul>
      {contract.residual_risk && <div>Residual risk: {contract.residual_risk}</div>}
    </div>
  )
}

function Policy({ policy }) {
  if (policy.decision === 'execute') {
    return <div style={muted}>Policy: allows this after approval ({policy.risk_tier})</div>
  }
  return (
    <div style={strong}>
      Policy withholds this action: {policy.blocked_reason || policy.decision}
      {policy.detail ? ` (${policy.detail})` : ''}
    </div>
  )
}

function Recovery({ recovery }) {
  if (!recovery?.state) return null
  const samples = (recovery.samples || []).length
  return (
    <div data-testid="action-recovery" style={strong}>
      Recovery: {recovery.state}
      {recovery.attribution ? `, attributed to ${recovery.attribution}` : ''}
      {recovery.verdict ? ` - ${recovery.verdict}` : ''} ({samples} samples)
    </div>
  )
}

function Controls({ proposal: p, database, path, onDone }) {
  const toast = useToast()
  const [busy, setBusy] = useState(false)
  const [reason, setReason] = useState('')
  async function run(url, body, done) {
    setBusy(true)
    try {
      await post(url, body)
      toast.success(done)
      if (onDone) onDone()
    } catch (err) {
      toast.error(err.message)
    } finally {
      setBusy(false)
    }
  }
  const queue = p.approval?.queue_id
  const db = encodeURIComponent(database)
  if (p.state === 'proposed' && p.policy?.decision === 'execute') {
    return (
      <button type="button" data-testid="action-request" disabled={busy}
        className="rounded px-2 py-1" style={buttonStyle}
        onClick={() => run(`${path}/${encodeURIComponent(p.id)}/request`, null,
          'Approval requested')}>
        Request approval
      </button>
    )
  }
  if (p.state !== 'requested' || p.approval?.status !== 'pending' || !queue) return null
  return (
    <div className="flex flex-wrap items-center gap-2">
      <button type="button" data-testid="action-approve" disabled={busy}
        className="rounded px-2 py-1" style={buttonStyle}
        onClick={() => run(`/api/v1/actions/${queue}/approve?database=${db}`, null,
          'Approved; pg_sage rechecks the target and runs it')}>
        Approve
      </button>
      <input data-testid="action-deny-reason" aria-label="Reason for denying"
        className="rounded border px-1" style={boxStyle} value={reason}
        placeholder="Reason for denying" onChange={e => setReason(e.target.value)} />
      <button type="button" data-testid="action-deny" disabled={busy || !reason.trim()}
        className="rounded px-2 py-1" style={buttonStyle}
        onClick={() => run(`/api/v1/actions/${queue}/reject?database=${db}`,
          { reason: reason.trim() }, 'Denied')}>
        Deny
      </button>
    </div>
  )
}
