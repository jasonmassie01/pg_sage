import { citationNumbers, statusLabel } from '../../lib/ask'
import { CitationList, CitationMarkers } from './Citations'
import { AnswerActions } from './AnswerActions'

// Answer renders one Ask Sage answer from its structured fields (never the
// server's pre-rendered text, never as HTML): the question, a status badge,
// cited statements, what could not be verified, claims the citation filter
// dropped, the evidence list and any actions set in motion.

const muted = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const list = v => (Array.isArray(v) ? v : [])

const STATUS_COLORS = {
  answered: 'var(--green, #38a169)',
  not_observed: 'var(--text-secondary)',
}

export function Answer({ answer }) {
  const citations = list(answer.citations)
  const numbers = citationNumbers(citations)
  const byId = new Map(citations.map(c => [c.id, c]))
  return (
    <article data-testid={`ask-answer-${answer.id}`} className="rounded p-4 space-y-3"
      style={{ background: 'var(--bg-card)', border: '1px solid var(--border)' }}>
      <div className="flex items-start gap-3">
        <p data-testid="ask-question" className="flex-1 font-medium" style={strong}>
          {answer.question}
        </p>
        <StatusBadge status={answer.status} />
      </div>
      <StatusMessage answer={answer} />
      {list(answer.statements).map((s, i) => (
        <p key={i} data-testid="ask-statement" className="text-sm" style={strong}>
          {s.text}
          <CitationMarkers ids={s.citations} numbers={numbers} byId={byId} />
        </p>
      ))}
      <NotVerified items={list(answer.not_verified)} />
      <Dropped items={list(answer.dropped)} />
      <AnswerActions actions={list(answer.actions)} />
      <CitationList citations={citations} />
    </article>
  )
}

function StatusBadge({ status }) {
  const color = STATUS_COLORS[status] || 'var(--yellow, #d69e2e)'
  return (
    <span data-testid="ask-status" className="rounded px-2 py-0.5 text-xs"
      style={{ color, border: `1px solid ${color}` }}>
      {statusLabel(status)}
    </span>
  )
}

function statusMessage(answer) {
  switch (answer.status) {
    case 'budget_exhausted':
      return 'The daily Ask Sage token budget for you or this database is used up. '
        + 'Try again tomorrow or ask an admin to raise the budget.'
    case 'llm_unavailable':
      return 'No LLM is available. An LLM must be configured for Ask Sage to answer.'
    case 'incomplete':
      return `The answer was cut short (stop: ${answer.stop || 'unknown'}). `
        + 'Only the statements below were checked against evidence.'
    default:
      return ''
  }
}

function StatusMessage({ answer }) {
  const text = statusMessage(answer)
  if (!text) return null
  return <p data-testid="ask-status-message" className="text-sm" style={muted}>{text}</p>
}

function NotVerified({ items }) {
  if (!items.length) return null
  return (
    <div data-testid="ask-not-verified">
      <h3 className="text-xs font-semibold uppercase" style={muted}>Could not verify</h3>
      <ul className="list-disc ml-5 text-sm" style={muted}>
        {items.map((t, i) => <li key={i}>{t}</li>)}
      </ul>
    </div>
  )
}

function Dropped({ items }) {
  if (!items.length) return null
  return (
    <details data-testid="ask-dropped" className="text-sm">
      <summary className="cursor-pointer text-xs" style={muted}>
        {items.length} dropped
      </summary>
      <ul className="ml-5 mt-1 space-y-1">
        {items.map((d, i) => (
          <li key={i} data-testid="ask-dropped-item" style={muted}>
            <span className="rounded px-1 mr-2 text-xs"
              style={{ color: 'var(--red)', border: '1px solid var(--red)' }}>
              not verified
            </span>
            <s>{d.text}</s> <span className="text-xs">({d.reason})</span>
          </li>
        ))}
      </ul>
    </details>
  )
}
