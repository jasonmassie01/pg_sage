// AnswerActions shows what Ask Sage set in motion: proposals queued for a
// person to approve on the Actions page, facts proposed for a person to
// confirm on the Facts page, and investigations opened on the Cases page. Ask Sage can never execute or approve anything, so this
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

const FACT_TEXT = {
  proposed: 'Fact proposed',
  already_pending: 'Fact already proposed',
  refused: 'Fact refused',
  failed: 'Fact failed',
}

const TEXTS = { proposal: PROPOSAL_TEXT, fact: FACT_TEXT }

const LINKS = {
  proposal: ['#/actions', 'Review it on the Actions page (a person approves it there)'],
  fact: ['#/facts', 'Review it on the Facts page (a person confirms it there)'],
}

const PENDING = new Set(['queued', 'already_pending', 'opened', 'joined', 'proposed'])

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
  const table = Object.hasOwn(TEXTS, action.kind) ? TEXTS[action.kind] : INVESTIGATION_TEXT
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
      {ok && <ActionLink kind={action.kind} />}
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

function ActionLink({ kind }) {
  const [href, text] = Object.hasOwn(LINKS, kind)
    ? LINKS[kind] : ['#/cases', 'Follow it on the Cases page']
  return <a href={href} className="text-xs underline" style={accent}>{text}</a>
}
