// AnswerActions shows what Ask Sage set in motion: proposals queued for a
// person to approve on the Actions page, and investigations opened on the
// Cases page. Ask Sage can never execute or approve anything, so this
// component renders links only, never an approve or execute control.

const muted = { color: 'var(--text-secondary)' }
const accent = { color: 'var(--accent)' }

const PROPOSAL_TEXT = {
  queued: 'Proposal queued for approval',
  already_pending: 'Proposal already pending approval',
  blocked: 'Proposal blocked',
  refused: 'Proposal refused',
  failed: 'Proposal failed',
}

const INVESTIGATION_TEXT = {
  opened: 'Investigation opened',
  joined: 'Joined an open investigation',
  blocked: 'Investigation blocked',
  refused: 'Investigation refused',
  failed: 'Investigation failed',
}

const PENDING = new Set(['queued', 'already_pending', 'opened', 'joined'])

export function AnswerActions({ actions }) {
  if (!actions.length) return null
  return (
    <div data-testid="ask-actions" className="space-y-2">
      <h3 className="text-xs font-semibold uppercase" style={muted}>Actions</h3>
      {actions.map((a, i) => <ActionItem key={`${a.kind}-${a.id}-${i}`} action={a} />)}
    </div>
  )
}

function ActionItem({ action }) {
  const isProposal = action.kind === 'proposal'
  const table = isProposal ? PROPOSAL_TEXT : INVESTIGATION_TEXT
  const headline = Object.hasOwn(table, action.status)
    ? table[action.status] : `${action.kind}: ${action.status}`
  const ok = PENDING.has(action.status)
  return (
    <div data-testid="ask-action" className="rounded p-2 text-sm space-y-1"
      style={{ border: '1px solid var(--border)', background: 'var(--bg-card)' }}>
      <div style={{ color: ok ? 'var(--text-primary)' : 'var(--red)' }}>
        {headline}{action.id !== undefined && action.id !== '' ? ` (#${action.id})` : ''}
      </div>
      {action.reason && <div style={muted}>Reason: {action.reason}</div>}
      {isProposal && <ProposalDetail action={action} />}
      {ok && <ActionLink isProposal={isProposal} />}
    </div>
  )
}

function ProposalDetail({ action }) {
  return (
    <div className="space-y-1 text-xs" style={muted}>
      {action.verdict && <div>Verdict: {action.verdict}</div>}
      {action.sql && <SQL label="SQL" text={action.sql} />}
      {action.rollback_sql && <SQL label="Rollback SQL" text={action.rollback_sql} />}
      {action.rollback_class && <div>Rollback class: {action.rollback_class}</div>}
    </div>
  )
}

function SQL({ label, text }) {
  return (
    <div>
      <div>{label}</div>
      <pre className="whitespace-pre-wrap rounded p-2"
        style={{ background: 'var(--bg-hover)', color: 'var(--text-primary)' }}>
        {text}
      </pre>
    </div>
  )
}

function ActionLink({ isProposal }) {
  return isProposal
    ? <a href="#/actions" className="text-xs underline" style={accent}>
      Review it on the Actions page (a person approves it there)
    </a>
    : <a href="#/cases" className="text-xs underline" style={accent}>
      Follow it on the Cases page
    </a>
}
