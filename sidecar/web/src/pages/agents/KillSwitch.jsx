// The fleet kill switch (spec §6.10, §8.4): an admin types KILL AGENTS and
// a reason; every agent is frozen at once on every database and replica,
// and the report says, per database and replica, what was done and whether
// it was verified.
import { useState } from 'react'
import { KILL_PHRASE, killAgents, killReady } from './agentsApi'
import { Button, ErrorBox, TextInput, card, muted } from './ui'

function ReplicaLine({ r }) {
  const what = r.configured ? 'configured replica' : 'unconfigured standby'
  return (
    <li data-testid={`kill-replica-${r.name}`} className="text-xs">
      {r.name} ({what}): {r.backends_terminated} sessions ended,
      {r.logins_blocked ? ' logins blocked' : ' logins not blocked'},
      {r.verified ? ' verified' : ' not verified'}
      {r.note && <span style={muted}> — {r.note}</span>}
      {r.error && <span style={{ color: '#ef4444' }}> — {r.error}</span>}
    </li>
  )
}

function DatabaseLine({ db }) {
  return (
    <li data-testid={`kill-db-${db.name}`} className="text-sm mb-2">
      <strong>{db.name}</strong>: {db.roles_disabled} roles disabled,{' '}
      {db.backends_terminated} sessions ended, {db.statements_cancelled} statements
      cancelled, {db.approvals_cancelled} approvals cancelled,{' '}
      {db.verified ? 'verified' : 'not verified'}
      {db.error && <span style={{ color: '#ef4444' }}> — {db.error}</span>}
      {db.replicas?.length > 0 && (
        <ul className="ml-4 mt-1">
          {db.replicas.map(r => <ReplicaLine key={r.name} r={r} />)}
        </ul>
      )}
    </li>
  )
}

export function KillReport({ report }) {
  return (
    <div data-testid="agents-kill-report" className="rounded-lg p-4 mb-4"
      style={{ ...card, borderColor: report.verified ? 'var(--border)' : '#ef4444' }}>
      <p className="text-sm font-semibold mb-2" style={{ color: 'var(--text-primary)' }}>
        Kill {report.verified ? 'verified everywhere' : 'not verified everywhere'}:{' '}
        {report.principals?.length || 0} agents frozen, {report.tokens_revoked} tokens
        revoked, {report.approvals_cancelled} approvals cancelled.
      </p>
      {report.control_error && (
        <p className="text-sm mb-2" style={{ color: '#ef4444' }}>
          Control database: {report.control_error}
        </p>
      )}
      <ul>{(report.databases || []).map(db => <DatabaseLine key={db.name} db={db} />)}</ul>
    </div>
  )
}

function KillDialog({ onCancel, onDone }) {
  const [reason, setReason] = useState('')
  const [phrase, setPhrase] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  async function confirmKill() {
    setBusy(true)
    setError(null)
    try {
      onDone(await killAgents(reason.trim()))
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div data-testid="agents-kill-dialog" role="dialog" aria-label="Kill agents"
      className="rounded-lg p-4 mb-4" style={{ ...card, borderColor: '#dc2626' }}>
      <p className="text-sm mb-2" style={{ color: 'var(--text-primary)' }}>
        This freezes every agent now: their roles stop logging in, their sessions end on
        every database and configured replica, their tokens are revoked and their
        pending approvals are cancelled. Unfreezing an agent afterwards needs two admins.
        Type <strong>{KILL_PHRASE}</strong> to confirm.
      </p>
      <div className="flex flex-wrap gap-2 mb-2">
        <TextInput testId="agents-kill-reason" value={reason} onChange={setReason}
          placeholder="Reason" />
        <TextInput testId="agents-kill-phrase" value={phrase} onChange={setPhrase}
          placeholder={KILL_PHRASE} label="Confirmation phrase" />
      </div>
      <ErrorBox testId="agents-kill-error" error={error} />
      <div className="flex gap-2">
        <Button testId="agents-kill-confirm" danger onClick={confirmKill}
          disabled={busy || !killReady(phrase, reason)}>
          Kill all agents
        </Button>
        <Button testId="agents-kill-cancel" onClick={onCancel}>Cancel</Button>
      </div>
    </div>
  )
}

export function KillSwitch({ onKilled }) {
  const [open, setOpen] = useState(false)
  const [report, setReport] = useState(null)
  function done(rep) {
    setOpen(false)
    setReport(rep)
    onKilled?.()
  }
  return (
    <div>
      <div className="flex justify-end mb-3">
        <Button testId="agents-kill-open" danger onClick={() => setOpen(true)}
          disabled={open}>
          Kill agents
        </Button>
      </div>
      {open && <KillDialog onCancel={() => setOpen(false)} onDone={done} />}
      {report && <KillReport report={report} />}
    </div>
  )
}
