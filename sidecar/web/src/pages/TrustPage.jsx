import { useState } from 'react'
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { TrustTable } from './trust/TrustTable'
import { ModelLift } from './trust/ModelLift'

// One trust system (roadmap 1.2): database x family x class. Each class
// pg_sage runs on its own initiative has one level per database, earned
// from verified outcomes and approved by an admin. trust.level is the
// ceiling and the trust ramp the minimum observation before a promotion
// is proposed; neither grants anything on its own.

const card = { background: 'var(--bg-card)', borderColor: 'var(--border)' }
const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }

async function approve(database, id) {
  const res = await fetch(
    `/api/v1/sre/autonomy/proposals/${id}/approve?database=${encodeURIComponent(database)}`,
    {
      method: 'POST', credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ note: 'approved on the Trust page' }),
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

export function TrustPage({ database, user }) {
  const query = database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}` : ''
  const trust = useAPI(`/api/v1/trust${query}`, 30000)
  const [actionError, setActionError] = useState(null)

  if (trust.loading) return <LoadingSpinner />
  if (trust.error) return <ErrorBanner message={trust.error} onRetry={trust.refetch} />

  const onApprove = async (db, id) => {
    setActionError(null)
    try {
      await approve(db, id)
      trust.refetch?.()
    } catch (err) {
      setActionError(err.message)
    }
  }
  const databases = trust.data?.databases || []
  return (
    <section className="space-y-4" data-testid="trust-page">
      <div>
        <h2 className="text-sm font-semibold" style={strong}>Trust</h2>
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
      {databases.map(view => (
        <DatabaseTrust key={view.database} view={view}
          isAdmin={user?.role === 'admin'} onApprove={onApprove} />
      ))}
      <ModelLift database={database} />
    </section>
  )
}

function DatabaseTrust({ view, isAdmin, onApprove }) {
  const rows = view.rows || []
  return (
    <div className="rounded border p-3 space-y-3" style={card}
      data-testid={`trust-db-${view.database}`}>
      <div className="flex flex-wrap items-baseline gap-3">
        <h3 className="text-sm font-semibold" style={strong}>{view.database}</h3>
        <Floor floor={view.floor} />
      </div>
      <Grandfathered database={view.database} record={view.grandfathered} />
      {['self_initiated', 'incident'].map(kind => (
        <TrustTable key={kind} database={view.database} kind={kind}
          rows={rows.filter(r => r.kind === kind)} isAdmin={isAdmin}
          onApprove={onApprove} shadow={view.shadow || []} />
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
      Observed {floor.observed} (promotion floor: L2 after {floor.required_l2}, L3
      after {floor.required_l3})
    </span>
  )
}

function Grandfathered({ database, record }) {
  if (!record) return null
  const seeded = record.seeded || []
  return (
    <div data-testid={`trust-grandfathered-${database}`} className="text-xs" style={muted}>
      The trust ledger took over on {new Date(record.migrated_at).toLocaleString()}.
      {seeded.length === 0
        ? ' Nothing ran unattended under the time ramp, so nothing was grandfathered.'
        : ' Kept from the time ramp (grandfathered, demotes like any level): '
          + seeded.map(s => `${s.family}/${s.class} ${s.level}`).join(', ')}
    </div>
  )
}
