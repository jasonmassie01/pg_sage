import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { useToast } from '../../components/Toast'

// Operator workflows restored from the legacy Findings and Incidents
// pages (SURF-10): suppress/unsuppress findings and resolve incidents,
// always against the case's own database.

const FINDING_SOURCES = new Set(['finding', 'schema_health', 'forecast'])

function canActOnCases(user) {
  return user?.role === 'admin' || user?.role === 'operator'
}

function dbQuery(name) {
  return name ? `?database=${encodeURIComponent(name)}` : ''
}

async function postAction(url, body) {
  const res = await fetch(url, {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({}))
    throw new Error(err.error || `Request failed (${res.status})`)
  }
}

function useCaseAction(onDone) {
  const toast = useToast()
  const [busy, setBusy] = useState(false)
  async function run(url, body, success) {
    setBusy(true)
    try {
      await postAction(url, body)
      toast.success(success)
      if (onDone) onDone()
    } catch (err) {
      toast.error(err.message)
    } finally {
      setBusy(false)
    }
  }
  return { busy, run }
}

const buttonStyle = {
  color: 'var(--text-primary)',
  border: '1px solid var(--border)',
}

export function CaseControls({ caseRow, user, onDone }) {
  const { busy, run } = useCaseAction(onDone)
  const sourceID = caseRow.source_ids?.[0]
  if (!canActOnCases(user) || !sourceID) return null
  const db = dbQuery(caseRow.database_name)
  const id = encodeURIComponent(sourceID)
  if (FINDING_SOURCES.has(caseRow.source_type)) {
    return (
      <button type="button" data-testid="case-suppress" disabled={busy}
        className="mt-3 rounded px-2 py-1 text-xs" style={buttonStyle}
        onClick={() => run(`/api/v1/findings/${id}/suppress${db}`, {},
          'Finding suppressed')}>
        Suppress
      </button>
    )
  }
  if (caseRow.source_type === 'incident') {
    return (
      <button type="button" data-testid="case-resolve" disabled={busy}
        className="mt-3 rounded px-2 py-1 text-xs" style={buttonStyle}
        onClick={() => run(`/api/v1/incidents/${id}/resolve${db}`,
          { reason: 'resolved from Cases' }, 'Incident resolved')}>
        Resolve incident
      </button>
    )
  }
  return null
}

export function SuppressedFindings({ database, user, onDone }) {
  const sep = database && database !== 'all'
    ? `&database=${encodeURIComponent(database)}` : ''
  const { data, error, refetch } = useAPI(
    `/api/v1/findings?status=suppressed&limit=100${sep}`, 0)
  const { busy, run } = useCaseAction(() => {
    refetch()
    if (onDone) onDone()
  })
  if (error) return <p className="text-xs">{error}</p>
  const findings = data?.findings || []
  if (findings.length === 0) {
    return <p className="text-xs" style={{ color: 'var(--text-secondary)' }}>
      No suppressed findings.
    </p>
  }
  return (
    <ul className="space-y-1" aria-label="Suppressed findings">
      {findings.map(f => (
        <li key={`${f.database_name || ''}:${f.id}`}
          className="flex items-center gap-2 text-xs">
          <span className="flex-1">{f.title}</span>
          {f.database_name && <span>{f.database_name}</span>}
          {canActOnCases(user) && (
            <button type="button" data-testid="case-unsuppress"
              disabled={busy} className="rounded px-2 py-0.5"
              style={buttonStyle}
              onClick={() => run(`/api/v1/findings/${encodeURIComponent(
                f.id)}/unsuppress${dbQuery(f.database_name)}`, {},
              'Finding restored')}>
              Unsuppress
            </button>
          )}
        </li>
      ))}
    </ul>
  )
}
