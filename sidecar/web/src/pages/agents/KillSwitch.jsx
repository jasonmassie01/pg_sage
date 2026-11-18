// The kill switch (spec §6.10, §8.4): an admin picks the scope (every
// agent, one agent or one database, agentgov-07), types KILL AGENTS and a
// reason; the agents are frozen at once on every database and replica in
// scope, and the report says, per database and replica, what was done and
// whether it was verified.
import { useState } from 'react'
import { Dialog } from '../../components/Dialog'
import { KILL_PHRASE, killAgents, killBody, killReady } from './agentsApi'
import { Button, ErrorBox, TextInput, card, inputStyle, muted } from './ui'

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

const SCOPE_TEXT = {
  all: ['every agent', 'Kill all agents'],
  principal: ['one agent', 'Kill this agent'],
  database: ['one database', 'Kill agents on this database'],
}

function Picker({ testId, label, value, onChange, options }) {
  return (
    <label className="text-sm" style={muted}>
      {label}{' '}
      <select data-testid={testId} value={value} onChange={e => onChange(e.target.value)}
        className="px-2 py-1 rounded text-sm" style={inputStyle}>
        {options.map(([v, text]) => <option key={v} value={v}>{text}</option>)}
      </select>
    </label>
  )
}

function ScopePicker({ form, set, agents, databases }) {
  const live = (agents || []).filter(a => a.status !== 'retired')
  return (
    <div className="flex flex-wrap gap-2 mb-2">
      <Picker testId="agents-kill-scope" label="Scope" value={form.scope}
        onChange={v => set({ scope: v, target: '' })}
        options={Object.entries(SCOPE_TEXT).map(([v, [text]]) => [v, text])} />
      {form.scope === 'principal' && (
        <Picker testId="agents-kill-agent" label="Agent" value={form.target}
          onChange={v => set({ target: v })}
          options={[['', 'choose an agent'], ...live.map(a => [a.id, a.name])]} />
      )}
      {form.scope === 'database' && (
        <Picker testId="agents-kill-database" label="Database" value={form.target}
          onChange={v => set({ target: v })}
          options={[['', 'choose a database'], ...databases.map(d => [d, d])]} />
      )}
    </div>
  )
}

function KillDialog({ agents, databases, onCancel, onDone }) {
  const [form, setForm] = useState({ scope: 'all', target: '', reason: '', phrase: '' })
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState(null)
  const set = patch => setForm(prev => ({ ...prev, ...patch }))
  async function confirmKill() {
    setBusy(true)
    setError(null)
    try {
      onDone(await killAgents(killBody(form)))
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }
  const ready = killReady(form.phrase, form.reason, form.scope, form.target)
  return (
    <Dialog open title="Kill agents" onClose={onCancel} busy={busy}
      testId="agents-kill-dialog" width={560}>
      <p className="text-sm" style={{ color: 'var(--text-primary)' }}>
        This freezes the agents in scope now: their roles stop logging in, their sessions end
        on every database and configured replica, their tokens are revoked and their
        pending approvals are cancelled. Unfreezing an agent, or releasing a fleet or
        database freeze, afterwards needs two admins. Type <strong>{KILL_PHRASE}</strong> to
        confirm.
      </p>
      <ScopePicker form={form} set={set} agents={agents} databases={databases} />
      <div className="flex flex-wrap gap-2 mb-2">
        <TextInput testId="agents-kill-reason" value={form.reason}
          onChange={v => set({ reason: v })} placeholder="Reason" />
        <TextInput testId="agents-kill-phrase" value={form.phrase}
          onChange={v => set({ phrase: v })} placeholder={KILL_PHRASE}
          label="Confirmation phrase" />
      </div>
      <ErrorBox testId="agents-kill-error" error={error} />
      <div className="flex gap-2">
        <Button testId="agents-kill-confirm" danger onClick={confirmKill}
          disabled={busy || !ready}>
          {SCOPE_TEXT[form.scope][1]}
        </Button>
        <Button testId="agents-kill-cancel" onClick={onCancel} disabled={busy}>
          Cancel
        </Button>
      </div>
    </Dialog>
  )
}

export function KillSwitch({ agents = [], databases = [], onKilled }) {
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
        <Button testId="agents-kill-open" danger onClick={() => setOpen(true)}>
          Kill agents
        </Button>
      </div>
      {open && <KillDialog agents={agents} databases={databases}
        onCancel={() => setOpen(false)} onDone={done} />}
      {report && <KillReport report={report} />}
    </div>
  )
}
