/* eslint-disable react-refresh/only-export-components */
import { LiveTimeAgo } from '../../components/TimeAgo'
import { SQLBlock } from '../../components/SQLBlock'
import { ApprovalCardDetail } from './ApprovalCards'
import { revisionPin } from './recommendation'
import { blockerText, rowDatabase } from './workflow'
import { actionRisk, actionRowKey, lifecycleStatus } from './actionFormat'
import { CaseRelation } from './CaseRelation'
import { LifecycleDetails } from './LifecycleDetails'
import { SqlSection } from './SqlSection'

// The columns and expanded detail of one queued action in the table view of
// Waiting on you (split out of Actions.jsx by dashboard-31).

const PENDING_RISKS = new Map([
  ['safe', { label: 'Low Risk', color: 'var(--green)' }],
  ['moderate', { label: 'Moderate Risk', color: 'var(--yellow)' }],
  ['high', { label: 'High Risk', color: 'var(--red)' }],
  ['high_risk', { label: 'High Risk', color: 'var(--red)' }],
])

function PendingRisk({ row }) {
  const risk = actionRisk(row)
  const info = PENDING_RISKS.get(risk) || { label: risk, color: 'var(--text-secondary)' }
  return <span style={{ color: info.color }}>{info.label}</span>
}

function Availability({ row, database, modeForDatabase }) {
  const reason = modeForDatabase?.(rowDatabase(row, database)) === 'observation'
    ? 'Database changes are disabled in Observation mode.'
    : blockerText(row.defer_reason || row.blocked_reason)
  return <span style={{ color: reason ? 'var(--yellow)' : 'var(--text-secondary)' }}>
    {reason || (row.eligible === false ? 'Cannot run yet; eligibility reason unavailable.'
      : 'Awaiting your decision; safety checks run again before execution.')}
  </span>
}

function sqlPreview(r) {
  const sql = (r.proposed_sql || '').replace(/\s+/g, ' ').trim()
  return sql.length > 90 ? sql.slice(0, 90) + '...' : sql
}

const BUTTON = 'px-2 py-1 rounded text-xs'

function DecisionCell({ row, database, modeForDatabase, decision }) {
  const windowOnly = row.policy_detail?.window_only === true
  const open = decision.rejectId === actionRowKey(row)
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap gap-2">
        <button onClick={() => decision.approve(row)} data-testid="approve-button"
          disabled={(row.eligible === false && !windowOnly) ||
            modeForDatabase?.(rowDatabase(row, database)) === 'observation'}
          title={row.eligible === false
            ? (row.defer_reason || row.blocked_reason || 'Action is not eligible') : undefined}
          className={BUTTON} style={{ background: 'var(--green-solid)', color: '#fff' }}>
          {row.eligible === false && windowOnly ? 'Approve for the next window' : 'Approve and run'}
        </button>
        <button onClick={() => decision.toggleReject(row)}
          data-testid="reject-button" className={BUTTON}
          style={{ background: 'var(--red-solid)', color: '#fff' }}>Reject</button>
      </div>
      {open && <div className="flex flex-wrap gap-2">
        <input value={decision.rejectReason} aria-label="Reason for rejection"
          onChange={e => decision.setRejectReason(e.target.value)}
          placeholder="Reason for rejection"
          className="px-2 py-1 rounded text-sm" style={{ background: 'var(--bg-primary)',
            color: 'var(--text-primary)', border: '1px solid var(--border)' }} />
        <button onClick={() => decision.reject(row)} disabled={!decision.rejectReason.trim()}
          className={BUTTON} style={{ background: 'var(--red-solid)', color: '#fff' }}>
          Confirm Reject</button>
      </div>}
    </div>
  )
}

// pendingColumns adds the Recommendation, Policy and Lifecycle columns only
// when a queued row carries them.
export function pendingColumns({ actions, database, modeForDatabase, decision }) {
  const optional = []
  if (actions.some(r => r.recommendation_id)) {
    optional.push({ key: 'recommendation_id', label: 'Saved change', render: revisionPin })
  }
  if (actions.some(r => r.policy_decision)) {
    optional.push({ key: 'policy_decision', label: 'Policy' })
  }
  if (actions.some(r => r.lifecycle_state || r.cooldown_until)) {
    optional.push({ key: 'lifecycle_state', label: 'Lifecycle', render: lifecycleStatus })
  }
  return [
    { key: 'action_risk', label: 'Risk', render: r => <PendingRisk row={r} /> },
    { key: 'database_name', label: 'Database' },
    { key: 'finding_id', label: 'Issue' },
    ...optional,
    { key: 'availability', label: 'Availability', render: r =>
      <Availability row={r} database={database} modeForDatabase={modeForDatabase} /> },
    { key: 'proposed_sql', label: 'SQL Preview', render: sqlPreview },
    { key: 'proposed_at', label: 'Proposed',
      render: r => <LiveTimeAgo timestamp={r.proposed_at} /> },
    { key: 'actions', label: 'Decision', render: r => <DecisionCell row={r}
      database={database} modeForDatabase={modeForDatabase} decision={decision} /> },
  ]
}

function ScriptPart({ title, children }) {
  return (
    <div className="mt-2">
      <div className="text-xs font-medium mb-1"
        style={{ color: 'var(--text-secondary)' }}>
        {title}
      </div>
      {children}
    </div>
  )
}

function ScriptOutput({ output }) {
  const verification = output.verification_sql || []
  return (
    <div>
      <div className="text-xs font-medium mb-1" style={{ color: 'var(--text-secondary)' }}>
        Migration script
      </div>
      <div className="text-xs mb-2" style={{ color: 'var(--text-secondary)' }}>
        {output.filename}
      </div>
      <SQLBlock sql={output.migration_sql} />
      {output.rollback_sql && <ScriptPart title="Rollback script">
        <SQLBlock sql={output.rollback_sql} /></ScriptPart>}
      {verification.length > 0 && <ScriptPart title="Verification SQL">
        {verification.map(sql => <SQLBlock key={sql} sql={sql} />)}</ScriptPart>}
      {(output.pr_title || output.pr_body) && (
        <div className="mt-2 text-xs" style={{ color: 'var(--text-secondary)' }}>
          <div className="font-medium" style={{ color: 'var(--text-primary)' }}>
            PR / CI output
          </div>
          {output.pr_title && <div>{output.pr_title}</div>}
          {output.pr_body && <div>{output.pr_body}</div>}
        </div>
      )}
    </div>
  )
}

// PendingDetail is the expanded view of one queued action.
export function PendingDetail({ row, cases, database, modeForDatabase, onDecided }) {
  return (
    <div className="space-y-3">
      <CaseRelation row={row} cases={cases} database={database} />
      <ApprovalCardDetail action={row} onDecided={onDecided}
        mode={modeForDatabase?.(rowDatabase(row, database))} />
      <SqlSection title="Proposed SQL" sql={row.proposed_sql} />
      {row.rollback_sql && <SqlSection title="Rollback SQL" sql={row.rollback_sql} />}
      {row.script_output && <ScriptOutput output={row.script_output} />}
      <LifecycleDetails row={row} />
    </div>
  )
}
