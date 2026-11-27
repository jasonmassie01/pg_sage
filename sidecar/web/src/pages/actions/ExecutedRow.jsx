/* eslint-disable react-refresh/only-export-components */
import { LiveTimeAgo } from '../../components/TimeAgo'
import { VerificationOutcome } from '../../components/VerificationOutcome'
import { isQueuedRow, queuedLabels, showsRollbackReason } from './ledger'
import { RollbackButton } from './RollbackControl'
import { actionLabel, riskLabel } from './workflow'
import { actionRisk, actionStatus, verificationStatus } from './actionFormat'
import { CaseRelation } from './CaseRelation'
import { SqlSection } from './SqlSection'

// The columns and expanded detail of one change record in the In progress
// and History lists (split out of Actions.jsx by dashboard-31).

const GREY = { bg: 'rgba(107,114,128,0.15)', color: 'var(--text-secondary)' }
const OUTCOME_STYLES = new Map([
  ['success', { bg: 'rgba(34,197,94,0.15)', color: 'var(--green)', label: 'Success' }],
  ['failed', { bg: 'rgba(239,68,68,0.15)', color: 'var(--red)', label: 'Failed' }],
  ['expired', { ...GREY, label: 'Expired' }],
  ['rejected', { ...GREY, label: 'Rejected' }],
  ['rolled_back', { bg: 'rgba(245,158,11,0.15)', color: 'var(--yellow)',
    label: 'Rolled Back' }],
  ['pending', { bg: 'rgba(59,130,246,0.15)', color: 'var(--blue, #3b82f6)',
    label: 'Monitoring' }],
])

// outcomeStyle returns a fresh copy, so a caller may relabel it.
function outcomeStyle(outcome) {
  const known = OUTCOME_STYLES.get(outcome)
  return known ? { ...known } : { ...GREY, label: outcome || 'Unknown' }
}

const SUMMARY_VERBS = new Map([
  ['drop_index', t => `Dropped index${t ? ' ' + t : ''}`],
  ['create_index', t => `Created index${t ? ' on ' + t : ''}`],
  ['vacuum', t => `Vacuumed${t ? ' ' + t : ' table'}`],
  ['analyze', t => `Updated statistics${t ? ' for ' + t : ''}`],
  ['reindex', t => `Reindexed${t ? ' ' + t : ''}`],
  ['alter', t => `Altered${t ? ' ' + t : ' object'}`],
])

function actionSummary(r) {
  if (isQueuedRow(r)) return `Proposed: ${actionLabel(r.action_type)}`
  if (r.outcome === 'failed' && r.rollback_reason) {
    const short = r.rollback_reason.length > 80
      ? r.rollback_reason.slice(0, 80) + '...'
      : r.rollback_reason
    return <span style={{ color: 'var(--red)' }}>{short}</span>
  }
  if ((r.outcome === 'expired' || r.outcome === 'rejected') && r.rollback_reason) {
    return r.rollback_reason
  }
  const target = (r.sql_executed || '').match(/(?:public\.)?([\w.]+)\s*[;(]/i)?.[1] || ''
  const verb = SUMMARY_VERBS.get((r.action_type || '').toLowerCase())
  if (verb) return verb(target)
  const raw = r.action_type || ''
  return raw.charAt(0).toUpperCase() + raw.slice(1)
}

const RISK_COLORS = new Map([['safe', 'var(--green)'], ['moderate', '#b58900'],
  ['high', 'var(--red)'], ['high_risk', 'var(--red)']])

function OutcomeBadge({ row }) {
  const s = outcomeStyle(actionStatus(row))
  if (isQueuedRow(row)) s.label = queuedLabels[actionStatus(row)] || s.label
  return (
    <span className="px-2 py-0.5 rounded-full text-xs
      font-medium inline-block"
      style={{ background: s.bg, color: s.color }}>
      {s.label}
    </span>
  )
}

function RiskBadge({ row }) {
  const risk = actionRisk(row)
  const c = RISK_COLORS.get(risk) || 'var(--text-secondary)'
  return (
    <span className="px-2 py-0.5 rounded-full text-xs font-medium
      inline-block"
      title="risk tier; executed rows are only logged here, queued rows await approval"
      style={{ border: `1px solid ${c}`, color: c }}>
      {riskLabel(risk)}
    </span>
  )
}

function Attempts({ row }) {
  const n = Number(row.attempts) || 1
  return n > 1
    ? <span title="times pg_sage attempted this action"
      style={{ color: 'var(--amber, #b58900)' }}>{n}×</span>
    : <span style={{ color: 'var(--text-secondary)' }}>1</span>
}

function typeLabel(r) {
  const t = (r.action_type || '').replace(/_/g, ' ')
  return t.charAt(0).toUpperCase() + t.slice(1)
}

// executedColumns adds the Attempts, Verification and Database columns only
// when a loaded row carries them.
export function executedColumns(actions) {
  const optional = []
  if (actions.some(r => Number(r.attempts) > 1)) {
    optional.push({ key: 'attempts', label: 'Attempts', render: r => <Attempts row={r} /> })
  }
  if (actions.some(r => r.verification_status || r.verification_outcome)) {
    optional.push({ key: 'verification_status', label: 'Verification',
      render: r => verificationStatus(r) })
  }
  if (actions.some(r => r.database_name)) {
    optional.push({ key: 'database_name', label: 'Database' })
  }
  return [
    { key: 'action_type', label: 'Type', render: typeLabel },
    { key: 'summary', label: 'Summary', render: actionSummary },
    { key: 'outcome', label: 'Outcome', render: r => <OutcomeBadge row={r} /> },
    { key: 'action_risk', label: 'Risk', render: r => <RiskBadge row={r} /> },
    ...optional,
    { key: 'executed_at', label: 'When',
      render: r => <LiveTimeAgo timestamp={r.executed_at} /> },
  ]
}

function ReasonBox({ title, color, tone, children }) {
  return (
    <div className="p-3 rounded text-sm"
      style={{ background: `rgba(${tone},0.1)`, border: `1px solid rgba(${tone},0.3)` }}>
      <div className="text-xs font-medium mb-1" style={{ color }}>
        {title}
      </div>
      {children}
    </div>
  )
}

function DetailFooter({ row }) {
  return (
    <div className="flex gap-4 text-xs" style={{ color: 'var(--text-secondary)' }}>
      {row.finding_id && <span>Issue #{row.finding_id}</span>}
      {row.database_name && <span>Database: {row.database_name}</span>}
      {row.measured_at && <span>Verified: <LiveTimeAgo timestamp={row.measured_at} /></span>}
    </div>
  )
}

// ExecutedDetail is the expanded view of one change record.
export function ExecutedDetail({ row, cases, database, canRollback, onRollback }) {
  const proposed = isQueuedRow(row) || row.outcome === 'expired' || row.outcome === 'rejected'
  return (
    <div className="space-y-3">
      {row.outcome === 'failed' && row.rollback_reason && (
        <ReasonBox title="Error Details" color="var(--red)" tone="239,68,68">
          <code className="text-xs"
            style={{ color: 'var(--text-primary)', wordBreak: 'break-all' }}>
            {row.rollback_reason}
          </code>
        </ReasonBox>
      )}
      {showsRollbackReason(row) && (
        <ReasonBox title="Rollback Reason" color="var(--yellow)" tone="245,158,11">
          <span className="text-xs" style={{ color: 'var(--text-primary)' }}>
            {row.rollback_reason}
          </span>
        </ReasonBox>
      )}
      <SqlSection title={proposed ? 'Proposed SQL' : 'SQL Executed'} sql={row.sql_executed} />
      <CaseRelation row={row} cases={cases} database={database} />
      <VerificationOutcome outcome={row.verification_outcome} />
      {row.rollback_sql && <SqlSection title="Rollback SQL" sql={row.rollback_sql} />}
      {canRollback && <RollbackButton row={row} onOpen={onRollback} />}
      <DetailFooter row={row} />
    </div>
  )
}
