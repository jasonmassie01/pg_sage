import { useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { ErrorBanner } from '../../components/ErrorBanner'
import { EmptyState } from '../../components/EmptyState'
import { SkeletonRow } from '../../components/Skeleton'
import { SQLBlock } from '../../components/SQLBlock'
import { LiveTimeAgo } from '../../components/TimeAgo'
import { actionLabel, blockerText, caseHref, recoveryForRevision, riskLabel, rowDatabase, storedProcedureRestriction } from './workflow'

const muted = { color: 'var(--text-secondary)' }

export function RecommendationsTab({ database, items = [], loading, error, refetch,
  onReview, modeForDatabase = () => null, view = 'proposed' }) {
  if (loading) return <div className="space-y-2" data-testid="recommendations-loading">
    {[0, 1, 2].map(i => <SkeletonRow key={i} cols={4} />)}
  </div>
  if (error) return <ErrorBanner message={error} onRetry={refetch} />
  if (!items.length) return (
    <div className="space-y-2">
      <EmptyState message={view === 'proposed'
        ? 'No proposed changes are available in this view. A case may still need investigation before a supported change can be proposed.'
        : view === 'progress' ? 'No recommendation is currently being applied or verified.'
          : 'No recommendation outcomes are recorded in this view.'} />
      <a href={caseHref(null, database)} className="text-sm underline"
        style={{ color: 'var(--accent)' }}>Review Cases</a>
    </div>
  )
  return (
    <div data-testid="recommendations-table" className="space-y-2" aria-label="Proposed changes">
      {items.map(row => <RecommendationCard key={row.key || `${rowDatabase(row, database)}:${row.id}`}
        row={row} database={database} onReview={onReview}
        mode={modeForDatabase(rowDatabase(row, database))} />)}
    </div>
  )
}

function RecommendationCard({ row, database, mode, onReview }) {
  const linked = row.linkedCase
  const db = rowDatabase(row, database)
  const current = row.currentCandidate
  const context = current || row
  const blocked = mode === 'observation'
    ? 'Database changes are disabled in Observation mode.'
    : blockerText(context.blocked_reason || context.policy_decision?.blocked_reason)
  const title = row.title || actionLabel(row.action_type)
  const state = ({ proposed: 'Proposed', approved: 'Approved; waiting to run',
    applying: 'Applying change', applied: 'Applied; awaiting verification', verifying: 'Verifying result',
    verified: 'Verification completed', reverted: 'Reverted', inconclusive: 'Outcome inconclusive',
    superseded: 'Replaced by a newer proposal', failed: 'Failed', abandoned: 'Stopped after failed attempts' })[row.state] || row.state
  return (
    <article className="rounded border p-3 space-y-2" data-testid="proposed-change"
      style={{ borderColor: 'var(--border)', background: 'var(--bg-card)' }}>
      <div className="flex flex-wrap justify-between gap-2">
        <h3 className="font-medium">{title}</h3>
        <span className="text-xs" style={muted}>{state}</span>
      </div>
      <p className="text-sm" style={muted}>
        {db || 'Database unknown'}{row.target ? <> · <code>{row.target}</code></> : ''}
        {' · '}{row.storedRecommendation ? 'Saved proposal risk' : 'Risk'}: {riskLabel(row.risk_tier || row.action_risk)}
      </p>
      <p className="text-sm" style={muted}>
        {linked?.why ? `Case context: ${linked.why}` : row.reason
          || 'The reason and expected effect have not been supplied in this summary. Review the case evidence before choosing a change.'}
      </p>
      {current ? <section className="rounded border p-2 space-y-1" data-testid="current-proposal-context"
        style={{ borderColor: 'var(--border)' }}>
        <h4 className="text-xs font-medium">Current case guidance</h4>
        <p className="text-xs" style={muted}>Risk: {riskLabel(current.risk_tier || current.action_risk)}</p>
        <ProposalPlan row={current} />
        <p className="text-xs" style={muted}>
          This guidance may differ from the saved proposal. Review the exact SQL and recovery before deciding.
        </p>
      </section> : <ProposalPlan row={row} /> }
      {blocked && <p className="text-sm" data-testid="proposal-blocker"
        style={{ color: 'var(--yellow)' }}>{current ? 'Current case proposal: ' : ''}{blocked}</p>}
      {!blocked && row.state === 'proposed' && <p className="text-sm" style={muted}>
        {row.pendingAction ? 'A request is waiting for review. Approval rechecks current safety conditions before running SQL.'
          : 'This is a proposal, not a scheduled change. Execution availability has not been confirmed.'}
      </p>}
      <div className="flex flex-wrap gap-3 items-center text-sm">
        {linked ? <a href={caseHref(linked, db)} className="underline"
          style={{ color: 'var(--accent)' }}>Review case: {linked.title}</a>
          : <span style={muted}>Case relation unavailable. <a href={caseHref(null, db)}
            className="underline" style={{ color: 'var(--accent)' }}>View Cases for this database</a></span>}
        {row.pendingAction && onReview && <button type="button" onClick={onReview}
          className="underline" style={{ color: 'var(--accent)' }}>Review request</button>}
      </div>
      <RecommendationDetails row={row} database={db} />
    </article>
  )
}

function ProposalPlan({ row }) {
  return <>
    <p className="text-xs" style={muted} data-testid="proposal-recovery">
      Recovery: {row.recovery_detail || ({ reversible: 'Marked reversible; review the recovery procedure and its limits in technical details.',
        forward_fix_only: 'Requires a follow-up repair; the original change cannot simply be undone.',
        no_rollback_needed: 'No database rollback is needed for this action.' })[row.rollback_class]
        || 'A complete recovery procedure has not been established in this summary.'}
    </p>
    <p className="text-xs" style={muted}>
      Verification: {row.verification_plan?.length ? row.verification_plan.join('; ')
        : 'No outcome-checking plan has been supplied in this summary.'}
    </p>
  </>
}

function RecommendationDetails({ row, database }) {
  const [open, setOpen] = useState(false)
  const detail = useAPI(open && row.id && database
    ? `/api/v1/recommendations/${encodeURIComponent(row.id)}?database=${encodeURIComponent(database)}` : null, 0)
  const revision = detail.data?.revisions?.find(r => r.revision === row.revision)
  const recovery = recoveryForRevision(row, revision)
  const restriction = storedProcedureRestriction(row)
  const current = row.currentCandidate || (!row.storedRecommendation ? row : null)
  const sameForward = Boolean(revision?.forward_sql && current?.proposed_sql
    && revision.forward_sql.trim() === current.proposed_sql.trim())
  return (
    <details onToggle={e => setOpen(e.currentTarget.open)} className="text-xs">
      <summary className="cursor-pointer" style={muted}>Technical details and history</summary>
      <div className="mt-2 space-y-2" style={muted}>
        {row.id && <p>Recommendation #{row.id} · r{row.revision} · Category: {row.category}</p>}
        {row.approved_by && <p>Approved by {row.approved_by} (r{row.approved_revision})</p>}
        {row.attempt_count != null && <p>Attempts: {row.attempt_count}/{Number(row.retry_budget || 0) + 1}</p>}
        {row.verdict && <p>Verification verdict: {row.verdict}</p>}
        {row.updated_at && <p>Updated <LiveTimeAgo timestamp={row.updated_at} /></p>}
        {detail.loading && <p>Loading exact proposal…</p>}
        {detail.error && <ErrorBanner message={detail.error} onRetry={detail.refetch} />}
        {revision?.recommendation && <p>Stored revision explanation: {revision.recommendation}</p>}
        {revision?.action_risk && <p>Stored revision risk: {riskLabel(revision.action_risk)}</p>}
        {revision?.forward_sql && restriction && <p data-testid="stored-procedure-withheld"
          style={{ color: 'var(--yellow)' }}>{restriction}</p>}
        {revision?.forward_sql && !restriction && <div data-testid="stored-proposal-sql">
          <p>Stored recommendation r{row.revision}: proposed SQL</p>
          <SQLBlock sql={revision.forward_sql} />
        </div>}
        {sameForward && !restriction && <p>The stored SQL text matches the current case proposal. Current context does not validate the stored revision's risk, recovery or eligibility.</p>}
        {current?.proposed_sql && !sameForward && !restriction && <div data-testid="current-proposal-sql">
          <p>Current case proposal: SQL</p><SQLBlock sql={current.proposed_sql} />
        </div>}
        {current?.recovery_detail && <p>Current proposal recovery limits: {current.recovery_detail}</p>}
        {current?.rollback_class && <p>Current proposal recovery classification: {current.rollback_class.replaceAll('_', ' ')}</p>}
        {recovery.reason && <p data-testid="stored-recovery-withheld" style={{ color: 'var(--yellow)' }}>{recovery.reason}</p>}
        {recovery.sql && <div data-testid="stored-recovery-sql">
          <p>Stored recovery SQL matches the current reversible procedure — review before use</p>
          <SQLBlock sql={recovery.sql} />
        </div>}
        {current?.rollback_class === 'reversible' && current.script_output?.rollback_sql && !recovery.sql && !restriction && <div>
          <p>Current case proposal: recovery SQL — does not validate a different stored revision</p>
          <SQLBlock sql={current.script_output.rollback_sql} />
        </div>}
        {current?.verification_plan?.length > 0 && <p>Current proposal verification: {current.verification_plan.join('; ')}</p>}
        {detail.data?.transitions?.map(t => <p key={t.id}>{t.from_state || 'New'} → {t.to_state}: {t.reason}</p>)}
      </div>
    </details>
  )
}
