import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { useToast } from '../../components/Toast'

// Managed clouds (roadmap phase 3): settings SQL cannot change on RDS,
// Aurora or Cloud SQL are proposed as the exact parameter-group or
// database-flag change. Each card shows the command, the reboot it needs
// and the rollback. Approving records that the operator will run it;
// pg_sage never applies these changes and marks one applied once
// PostgreSQL runs the new value.

const OPEN = new Set(['pending', 'approved'])
const muted = { color: 'var(--text-secondary)' }
const codeStyle = { background: 'var(--bg-primary)', border: '1px solid var(--border)',
  color: 'var(--text-primary)', whiteSpace: 'pre-wrap', wordBreak: 'break-all' }

async function postDecision(database, id, verb) {
  const res = await fetch(
    `/api/v1/managed-changes/${id}/${verb}?database=${encodeURIComponent(database)}`, {
      method: 'POST', credentials: 'include',
      headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({}),
    })
  let json = {}
  try {
    json = await res.json()
  } catch (err) {
    json = { error: `unreadable response (${err.message})` }
  }
  return { ok: res.ok, status: res.status, json }
}

function Decision({ database, record, onDecided }) {
  const toast = useToast()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function decide(verb) {
    setBusy(true)
    setError('')
    try {
      const { ok, status, json } = await postDecision(database, record.id, verb)
      if (ok) {
        toast.success(verb === 'approve'
          ? 'Approved: run the command; pg_sage verifies the running value'
          : 'Rejected: pg_sage will not propose it again for a week')
        onDecided?.()
      } else {
        setError(json.error || `Decision failed (${status})`)
      }
    } catch (err) {
      setError(`Request failed: ${err.message}`)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-1">
      <div className="flex flex-wrap gap-2">
        <button data-testid="managed-change-approve" disabled={busy}
          onClick={() => decide('approve')} className="px-3 py-1 rounded text-sm"
          style={{ background: 'var(--green)', color: '#fff', opacity: busy ? 0.5 : 1 }}>
          I will apply it
        </button>
        <button data-testid="managed-change-reject" disabled={busy}
          onClick={() => decide('reject')} className="px-3 py-1 rounded text-sm"
          style={{ background: 'var(--red)', color: '#fff', opacity: busy ? 0.5 : 1 }}>
          Reject
        </button>
      </div>
      {error && (
        <p data-testid="managed-change-error" className="text-xs"
          style={{ color: 'var(--red)' }}>{error}</p>
      )}
    </div>
  )
}

function Command({ testid, label, text }) {
  if (!text) return null
  return (
    <div data-testid={testid}>
      <div className="text-xs" style={muted}>{label}</div>
      <pre className="text-xs p-2 rounded" style={codeStyle}>{text}</pre>
    </div>
  )
}

function Card({ database, record, canDecide, onDecided }) {
  const p = record.proposal || {}
  const unit = p.unit ? ` ${p.unit}` : ''
  return (
    <div data-testid="managed-change-card" className="rounded p-3 space-y-2"
      style={{ border: '1px solid var(--border)', background: 'var(--bg-secondary)' }}>
      <div className="flex flex-wrap items-baseline gap-2">
        <span className="font-medium">Set {p.parameter} to {p.pg_value}</span>
        <span className="text-xs" style={muted}>
          {p.provider} {p.mechanism === 'database_flag' ? 'database flag' : 'parameter group'}
          {' '}{p.target} · value {p.value}{unit} · {record.status}
        </span>
      </div>
      <div data-testid="managed-change-reboot" className="text-xs"
        style={{ color: p.reboot_required ? 'var(--yellow)' : 'var(--text-secondary)' }}>
        {p.reboot_required
          ? 'Reboot required: the change is pending until the instance restarts (an outage)'
          : 'Applies immediately, no reboot'}
        {p.running_value ? ` · running now: ${p.running_value}${unit}` : ''}
      </div>
      {p.blockers?.length > 0 && (
        <ul data-testid="managed-change-blockers" className="text-xs list-disc pl-4"
          style={{ color: 'var(--red)' }}>
          {p.blockers.map(b => <li key={b}>{b}</li>)}
        </ul>
      )}
      <Command testid="managed-change-cli" label="Command" text={p.cli} />
      <Command testid="managed-change-rollback" label="Rollback" text={p.rollback?.cli} />
      {p.notes?.length > 0 && (
        <ul className="text-xs list-disc pl-4" style={muted}>
          {p.notes.map(n => <li key={n}>{n}</li>)}
        </ul>
      )}
      <p className="text-xs" style={muted}>
        {p.auto_apply_withheld}
        {p.console_url && (
          <> · <a href={p.console_url} target="_blank" rel="noreferrer noopener"
            style={{ color: 'var(--accent)' }}>open the console</a></>
        )}
      </p>
      {record.decision_note && (
        <p className="text-xs" style={muted}>Note: {record.decision_note}</p>
      )}
      {canDecide && record.status === 'pending' && (
        <Decision database={database} record={record} onDecided={onDecided} />
      )}
    </div>
  )
}

export function ManagedChanges({ database, canDecide }) {
  const dbParam = database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}` : ''
  const { data, refetch } = useAPI(`/api/v1/managed-changes${dbParam}`)
  const groups = (data?.databases || []).map(d => ({
    database: d.database,
    records: (d.proposals || []).filter(r => OPEN.has(r.status)),
  })).filter(g => g.records.length > 0)
  if (groups.length === 0) return null
  return (
    <section data-testid="managed-changes" className="space-y-2">
      <h3 className="text-sm font-medium">Provider changes (parameter groups and flags)</h3>
      <p className="text-xs" style={muted}>{data?.meaning}</p>
      {groups.map(g => g.records.map(r => (
        <Card key={`${g.database}-${r.id}`} database={g.database} record={r}
          canDecide={canDecide} onDecided={refetch} />
      )))}
    </section>
  )
}
