import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { TimeAgo } from '../../components/TimeAgo'
import { InvestigationPanel } from '../cases/InvestigationPanel'
import { humanize } from './sloText'
import { formatTime } from '../../lib/time'

// The expansion of one SLO row (playbooks-09): the SLO's state
// transitions, the recovery verdict since its burn started, and the
// newest slo:<name> investigation. Both reads are made once when the row
// opens (interval 0), never on the list's 30 s poll, and always name the
// row's database (fleet mode).

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

const NO_INVESTIGATION_PAGE =
  'No investigation opened (sre.slo.open_investigations or automatic_start off)'
const NO_INVESTIGATION = 'No investigation opened: only a page-level burn opens one.'

// investigationDB is the database whose investigator opens the SLO's
// slo:<name> investigations: a fleet SLO's owner_database, else its own.
function investigationDB(slo) {
  return slo.owner_database || slo.database
}

function detailURLs(slo) {
  const db = encodeURIComponent(slo.database)
  let detail = `/api/v1/sre/slos/${encodeURIComponent(slo.name)}?database=${db}`
  if (slo.burn_started_at) detail += `&since=${encodeURIComponent(slo.burn_started_at)}`
  const caseID = encodeURIComponent(`slo:${slo.name}`)
  const owner = encodeURIComponent(investigationDB(slo))
  return { detail, investigations: `/api/v1/investigations?database=${owner}` +
    `&case_id=${caseID}&limit=1` }
}

export function SLODetail({ slo, user }) {
  // Captured when the row opens: a list refresh does not read again.
  const [urls] = useState(() => detailURLs(slo))
  const detail = useAPI(urls.detail, 0)
  const investigations = useAPI(urls.investigations, 0)
  return (
    <div data-testid="slo-detail" className="p-2 space-y-2 text-xs">
      <History result={detail} />
      <Investigation result={investigations} slo={slo} user={user} />
    </div>
  )
}

function History({ result }) {
  const { data, loading, error } = result
  if (error) return <div style={muted}>SLO detail unavailable: {error}</div>
  if (loading && !data) return <div style={muted}>Loading SLO history...</div>
  if (!data) return null
  const transitions = data.transitions || []
  return (
    <div>
      <div className="font-medium mb-1" style={strong}>State changes</div>
      {transitions.length === 0 && <div style={muted}>No state changes recorded.</div>}
      <ul className="space-y-0.5">
        {transitions.map((t, i) => (
          <li key={`${t.at}-${i}`} data-testid="slo-transition" style={strong}
            title={formatTime(t.at)}>
            {t.from || 'first evaluation'} → {t.to} · <TimeAgo timestamp={t.at} />
          </li>
        ))}
      </ul>
      {data.recovery && <Recovery recovery={data.recovery} />}
    </div>
  )
}

function Recovery({ recovery }) {
  const reason = recovery.reason ? ` (${humanize(recovery.reason)})` : ''
  return (
    <div data-testid="slo-recovery" className="mt-1" style={strong}>
      Recovery: {humanize(recovery.state)}{reason}
      <span style={muted}> · {recovery.evaluated ?? 0} slices evaluated</span>
    </div>
  )
}

function Investigation({ result, slo, user }) {
  const { data, loading, error } = result
  if (error) return <div style={muted}>Investigation lookup failed: {error}</div>
  if (loading && !data) return <div style={muted}>Looking for an investigation...</div>
  if (!data) return null
  const db = investigationDB(slo)
  const newest = (data.items || [])[0]
  // An investigator that could not be read is not "none opened".
  if (!newest && (data.unavailable || []).includes(db)) {
    return (
      <div data-testid="slo-investigation-unavailable" style={muted}>
        Investigations of {db} unavailable: whether one is open is unknown.
      </div>
    )
  }
  return (
    <div>
      {db !== slo.database && (
        <div data-testid="slo-owner" style={muted}>
          Investigated on {db} (owner_database): only the owner opens one.
        </div>
      )}
      {newest ? (
        <InvestigationPanel database={newest.database || db}
          investigation={newest} user={user} />
      ) : (
        <div data-testid="slo-no-investigation" style={muted}>
          {slo.state === 'page' ? NO_INVESTIGATION_PAGE : NO_INVESTIGATION}
        </div>
      )}
    </div>
  )
}
