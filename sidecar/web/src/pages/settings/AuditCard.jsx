/* eslint-disable react-refresh/only-export-components */
import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { QueryState } from '../../components/QueryState'
import { queryBlocked } from '../../lib/queryState'

// Audit trail (audit-12): the last scheduled verification of every audit
// chain, SIEM delivery health and the evidence-pack download, from
// GET /api/v1/audit/status. The status never verifies on demand; the
// scheduled verification runs on the fleet leader only.

const STATUS_URL = '/api/v1/audit/status'
const muted = { color: 'var(--text-secondary)' }
const rowStyle = { borderTop: '1px solid var(--border)' }
const inputStyle = {
  background: 'var(--bg-main)', border: '1px solid var(--border)', color: 'var(--text-primary)',
}

const ACTION_ID = /^[1-9][0-9]*$/
const LOCAL_TIME = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?$/

// utc reads a datetime-local value as UTC, the zone the form names.
function utc(value) {
  if (!LOCAL_TIME.test(value)) return null
  return value.length === 16 ? `${value}:00Z` : `${value}Z`
}

// evidenceURL is the download of one action's pack or a window's, or null
// while the scope is incomplete or invalid.
export function evidenceURL({ database, actionID, from, to }) {
  const db = (database || '').trim()
  if (!db) return null
  const base = `/api/v1/audit/evidence?database=${encodeURIComponent(db)}`
  const id = (actionID || '').trim()
  if (id) return ACTION_ID.test(id) ? `${base}&action_id=${id}` : null
  const start = utc(from || '')
  const end = utc(to || '')
  if (!start || !end) return null
  return `${base}&from=${encodeURIComponent(start)}&to=${encodeURIComponent(end)}`
}

function problemText(problems) {
  return Object.entries(problems || {})
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([kind, n]) => `${kind} ×${n}`)
    .join(', ')
}

function ChainRow({ chain: c }) {
  const problems = problemText(c.problems)
  return (
    <div data-testid={`audit-chain-${c.database}-${c.chain}`} data-ok={String(c.ok)}
      className="py-2 text-xs flex flex-wrap gap-2 items-center" style={rowStyle}>
      <span style={{ color: 'var(--text-primary)' }}>{c.database}</span>
      <code>{c.chain}</code>
      <span data-testid="audit-chain-state" className="font-semibold"
        style={{ color: c.ok ? 'var(--green)' : 'var(--red)' }}>
        {c.ok ? 'verified' : 'fails verification'}
      </span>
      {problems && <span style={{ color: 'var(--red)' }}>{problems}</span>}
      {c.problems_truncated && <span style={muted}>(more not listed)</span>}
      <span style={muted}>
        {c.mode} · head {c.head_seq} · last verified {c.verified_at}
      </span>
    </div>
  )
}

function Chains({ data }) {
  const chains = Array.isArray(data.chains) ? data.chains : []
  if (chains.length === 0) {
    const hours = data.verify_interval_hours
    return (
      <div data-testid="audit-not-verified" className="text-xs" style={muted}>
        {hours > 0
          ? `No chain verified since this sidecar started. Scheduled verification runs `
            + `every ${hours} h, on the fleet leader only.`
          : 'Scheduled verification is off (audit.verify_interval_hours is 0).'}
      </div>
    )
  }
  return chains.map(c => <ChainRow key={`${c.database}/${c.chain}`} chain={c} />)
}

function VerifyErrors({ errors }) {
  if (!Array.isArray(errors) || errors.length === 0) return null
  return (
    <ul data-testid="audit-verify-errors" className="text-xs space-y-1"
      style={{ color: 'var(--yellow)' }}>
      {errors.map(e => (
        <li key={e.database}>
          {e.database}: {e.count} verification run(s) failed before finishing; last at{' '}
          {e.last_at}: {e.last_error}
        </li>
      ))}
    </ul>
  )
}

function SinkRow({ sink: s }) {
  const lag = (s.lanes || []).reduce((sum, l) => sum + (l.chain ? l.lag || 0 : 0), 0)
  const failing = s.status !== 'ok' || s.consecutive_failures > 0
  return (
    <div data-testid={`audit-sink-${s.name}`} className="py-2 text-xs" style={rowStyle}>
      <span style={{ color: 'var(--text-primary)' }}>{s.name}</span>{' '}
      <span style={{ color: failing ? 'var(--red)' : 'var(--green)' }}>{s.status}</span>
      {' · '}{s.consecutive_failures} consecutive failures · {lag} links behind
      {' · '}{s.delivered} delivered
      {s.last_error && <div style={muted}>{s.last_error}</div>}
    </div>
  )
}

function SIEM({ siem }) {
  if (!siem?.enabled) {
    return (
      <div data-testid="audit-siem-off" className="text-xs" style={muted}>
        SIEM export is off (audit.siem.sinks). Only an off-box copy, or signed
        checkpoints, catch a chain rewritten whole.
      </div>
    )
  }
  return (siem.sinks || []).map(s => <SinkRow key={s.name} sink={s} />)
}

function SigningState({ evidence }) {
  return (
    <div data-testid="audit-evidence-signed" className="text-xs"
      style={{ color: evidence?.signed ? 'var(--green)' : 'var(--yellow)' }}>
      {evidence?.signed
        ? `Signed (key ${evidence.key_id}, ${evidence.source})`
        : 'Unsigned: the hashes catch accidental corruption only. Set encryption_key '
          + 'or audit.evidence.signing_key_env to sign packs.'}
    </div>
  )
}

function Field({ label, testid, value, onChange, type = 'text' }) {
  return (
    <label className="text-xs flex flex-col gap-1" style={muted}>
      {label}
      <input data-testid={testid} type={type} value={value}
        onChange={e => onChange(e.target.value)} className="px-2 py-1 rounded text-xs"
        style={inputStyle} />
    </label>
  )
}

function EvidenceForm({ database, evidence }) {
  const [scope, setScope] = useState({
    database: database && database !== 'all' ? database : '', actionID: '', from: '', to: '',
  })
  const set = key => value => setScope(prev => ({ ...prev, [key]: value }))
  const url = evidenceURL(scope)
  return (
    <div className="space-y-2 pt-2" style={rowStyle}>
      <h3 className="text-xs font-semibold">Evidence pack</h3>
      <SigningState evidence={evidence} />
      <div className="flex flex-wrap gap-2 items-end">
        <Field label="Database" testid="audit-evidence-database" value={scope.database}
          onChange={set('database')} />
        <Field label="Action id" testid="audit-evidence-action" value={scope.actionID}
          onChange={set('actionID')} />
        <Field label="or from (UTC)" testid="audit-evidence-from" type="datetime-local"
          value={scope.from} onChange={set('from')} />
        <Field label="to (UTC)" testid="audit-evidence-to" type="datetime-local"
          value={scope.to} onChange={set('to')} />
        <a data-testid="audit-evidence-download" href={url ?? undefined}
          aria-disabled={String(!url)} download
          className="text-xs px-3 py-1 rounded"
          style={{ border: '1px solid var(--border)',
            color: url ? 'var(--accent)' : 'var(--text-secondary)' }}>
          Download
        </a>
      </div>
      <div className="text-xs" style={muted}>
        An action id exports one change; a window of at most 31 days exports every
        audit table. Verify a pack with pg_sage_sidecar evidence verify.
      </div>
    </div>
  )
}

export function AuditCard({ database }) {
  const status = useAPI(STATUS_URL, 60000)
  const message = e => `Audit trail: ${e}`
  if (queryBlocked(status)) {
    return <QueryState query={status} skeleton={null} message={message} />
  }
  const data = status.data || {}
  return (
    <QueryState query={status} message={message}>
      <section data-testid="audit-card" data-url={STATUS_URL}
        className="rounded p-4 space-y-2"
        style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
        <h2 className="text-sm font-semibold" style={{ color: 'var(--text-primary)' }}>
          Audit trail
        </h2>
        <p className="text-xs" style={muted}>
          Each audit table is hash-chained. The scheduled verification below proves no
          row was edited, deleted or written behind the chain since its last run.
        </p>
        <Chains data={data} />
        <VerifyErrors errors={data.verify_errors} />
        <h3 className="text-xs font-semibold pt-2">SIEM export</h3>
        <SIEM siem={data.siem} />
        <EvidenceForm database={database} evidence={data.evidence} />
      </section>
    </QueryState>
  )
}
