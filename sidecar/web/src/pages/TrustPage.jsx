import { useState } from 'react'
import { Term } from '../components/Term'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { QueryState } from '../components/QueryState'
import { queryBlocked } from '../lib/queryState'
import { TrustTable } from './trust/TrustTable'
import { ModelLift } from './trust/ModelLift'
import { TuningCalibration } from './trust/TuningCalibration'
import { formatTime } from '../lib/time'
import { summarizes } from '../lib/fleetScope'
import { TrustSummary } from './trust/TrustSummary'
import { WaitingPromotions } from './autonomy/WaitingPromotions'

// One trust system (roadmap 1.2): database x family x class. Each class
// pg_sage runs on its own initiative has one level per database, earned
// from verified outcomes and approved by an admin. trust.level is the
// ceiling and the trust ramp the minimum observation before a promotion
// is proposed; neither grants anything on its own.

const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }
const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

async function postAutonomy(path, database, body) {
  const res = await fetch(
    `/api/v1/sre/autonomy/${path}?database=${encodeURIComponent(database)}`,
    {
      method: 'POST', credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
  if (!res.ok) {
    let message = `${res.status}`
    try {
      const data = await res.json()
      message = data.error || message
    } catch {
      // keep the status as the message
    }
    throw new Error(message)
  }
}

const approve = (database, id, note) => postAutonomy(`proposals/${id}/approve`, database,
  { note: note || 'approved on the Trust page' })

const reject = (database, id, note) => postAutonomy(`proposals/${id}/reject`, database,
  { note: note || 'rejected on the Trust page' })

// earned-07: re-approve a self-initiated level capped by a PostgreSQL major
// upgrade (admin only, like a promotion).
const revalidate = (database, family, cls) => postAutonomy('revalidate', database,
  { family, class: cls, note: 're-approved on the Trust page' })

// In the "all" scope of a fleet of FLEET_SUMMARY_FROM or more the page
// shows level counts per class instead of one ledger per database (ux-10b).
export function TrustPage({ database, user, onSelectDatabase }) {
  const query = database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}` : ''
  const trust = useAPI(`/api/v1/trust${query}`, 30000)
  const [actionError, setActionError] = useState(null)

  if (queryBlocked(trust)) {
    return <QueryState query={trust} skeleton={<LoadingSpinner />} />
  }

  // Every decision re-reads the ledger, a refused one too: a second admin's
  // approval of a decided proposal shows the server's conflict and the row
  // as it now is (ux-06b).
  const act = fn => async (...args) => {
    setActionError(null)
    try {
      await fn(...args)
    } catch (err) {
      setActionError(err.message)
    } finally {
      trust.refetch?.()
    }
  }
  const onApprove = act(approve)
  const onReject = act(reject)
  const onRevalidate = act(revalidate)
  const databases = trust.data?.databases || []
  const role = user?.role
  return (
    <QueryState query={trust}><section className="space-y-4" data-testid="trust-page">
      <div>
        <h3 className="text-sm font-semibold" style={strong}>Each class</h3>
        <p className="text-sm" style={muted}>{trust.data?.meaning}</p>
        {trust.data && !trust.data.enforced && (
          <div data-testid="trust-not-enforced" className="mt-2 text-xs"
            style={{ color: 'var(--red)' }}>
            The trust ledger is not enforced (sre.autonomy.enforce: false): the
            elapsed-time ramp still grants autonomy.
          </div>
        )}
      </div>
      {actionError && (
        <div role="alert" className="text-sm" style={{ color: 'var(--red)' }}>
          {actionError}
        </div>
      )}
      {databases.length === 0 && (
        <div data-testid="trust-empty" className="text-sm" style={muted}>
          No database has a trust ledger yet: it is created when a database&apos;s
          executor starts.
        </div>
      )}
      {summarizes(database, databases.length)
        ? <TrustSummary views={databases} onSelectDatabase={onSelectDatabase} />
        : databases.map(view => (
          <DatabaseTrust key={view.database} view={view}
            isAdmin={role === 'admin'} onRevalidate={onRevalidate} />
        ))}
      <WaitingPromotions databases={databases} isAdmin={role === 'admin'}
        canReject={role === 'admin' || role === 'operator'}
        onApprove={onApprove} onReject={onReject} />
      <ModelLift database={database} />
      <TuningCalibration database={database} />
    </section></QueryState>
  )
}

function DatabaseTrust({ view, isAdmin, onRevalidate }) {
  const rows = view.rows || []
  return (
    <div className="rounded border p-3 space-y-3" style={card}
      data-testid={`trust-db-${view.database}`}>
      <div className="flex flex-wrap items-baseline gap-3">
        <h3 className="text-sm font-semibold" style={strong}>{view.database}</h3>
        <Floor floor={view.floor} />
      </div>
      <Grandfathered database={view.database} record={view.grandfathered} />
      {rows.length === 0 && (
        <div data-testid={`trust-no-classes-${view.database}`} className="text-xs"
          style={muted}>
          No action classes yet: the ledger lists every class once this
          database&apos;s executor starts.
        </div>
      )}
      {['self_initiated', 'incident'].map(kind => (
        <TrustTable key={kind} database={view.database} kind={kind}
          rows={rows.filter(r => r.kind === kind)} isAdmin={isAdmin}
          onRevalidate={onRevalidate} shadow={view.shadow || []} />
      ))}
    </div>
  )
}

function Floor({ floor }) {
  if (!floor?.known) {
    return <span className="text-xs" style={muted}>Observation start unknown</span>
  }
  return (
    <span className="text-xs" style={muted}>
      Observed {floor.observed} (<Term id="minimum-evidence">minimum evidence</Term>:
      L2 after {floor.required_l2}, L3
      after {floor.required_l3})
    </span>
  )
}

function Grandfathered({ database, record }) {
  if (!record) return null
  const seeded = record.seeded || []
  return (
    <div data-testid={`trust-grandfathered-${database}`} className="text-xs" style={muted}>
      The trust ledger took over on {formatTime(record.migrated_at)}.
      {seeded.length === 0
        ? ' Nothing ran unattended under the time ramp, so nothing was grandfathered.'
        : ' Kept from the time ramp (grandfathered, demotes like any level): '
          + seeded.map(s => `${s.family}/${s.class} ${s.level}`).join(', ')}
    </div>
  )
}
